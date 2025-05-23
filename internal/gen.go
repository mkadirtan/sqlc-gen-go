package golang

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/format"
	"slices"
	"strings"
	"text/template"

	"github.com/sqlc-dev/plugin-sdk-go/metadata"
	"github.com/sqlc-dev/plugin-sdk-go/plugin"
	"github.com/sqlc-dev/plugin-sdk-go/sdk"
	"github.com/sqlc-dev/sqlc-gen-go/internal/opts"
)

type tmplCtx struct {
	Q           string
	Package     string
	SQLDriver   opts.SQLDriver
	Enums       []Enum
	Structs     []Struct
	GoQueries   []Query
	SqlcVersion string

	// TODO: Race conditions
	SourceName string

	EmitJSONTags              bool
	JsonTagsIDUppercase       bool
	EmitDBTags                bool
	EmitPreparedQueries       bool
	EmitInterface             bool
	EmitEmptySlices           bool
	EmitNilRecords            bool
	EmitMethodsWithDBArgument bool
	EmitEnumValidMethod       bool
	EmitAllEnumValues         bool
	UsesCopyFrom              bool
	UsesBatch                 bool
	OmitSqlcVersion           bool
	BuildTags                 string
}

func (t *tmplCtx) OutputQuery(sourceName string) bool {
	return t.SourceName == sourceName
}

func (t *tmplCtx) codegenDbarg() string {
	if t.EmitMethodsWithDBArgument {
		return "db DBTX, "
	}
	return ""
}

// Called as a global method since subtemplate queryCodeStdExec does not have
// access to the toplevel tmplCtx
func (t *tmplCtx) codegenEmitPreparedQueries() bool {
	return t.EmitPreparedQueries
}

func (t *tmplCtx) codegenQueryMethod(q Query) string {
	db := "q.db"
	if t.EmitMethodsWithDBArgument {
		db = "db"
	}

	switch q.Cmd {
	case ":one":
		if t.EmitPreparedQueries {
			return "q.queryRow"
		}
		return db + ".QueryRowContext"

	case ":many":
		if t.EmitPreparedQueries {
			return "q.query"
		}
		return db + ".QueryContext"

	default:
		if t.EmitPreparedQueries {
			return "q.exec"
		}
		return db + ".ExecContext"
	}
}

func (t *tmplCtx) codegenQueryRetval(q Query) (string, error) {
	switch q.Cmd {
	case ":one":
		return "row :=", nil
	case ":many":
		return "rows, err :=", nil
	case ":exec":
		return "_, err :=", nil
	case ":execrows", ":execlastid":
		return "result, err :=", nil
	case ":execresult":
		return "return", nil
	default:
		return "", fmt.Errorf("unhandled q.Cmd case %q", q.Cmd)
	}
}

func Generate(ctx context.Context, req *plugin.GenerateRequest) (*plugin.GenerateResponse, error) {
	options, err := opts.Parse(req)
	if err != nil {
		return nil, err
	}

	if err := opts.ValidateOpts(options); err != nil {
		return nil, err
	}

	enums := buildEnums(req, options)
	structs := buildStructs(req, options)
	queries, err := buildQueries(req, options, structs)
	if err != nil {
		return nil, err
	}

	if options.OmitUnusedStructs {
		enums, structs = filterUnusedStructs(enums, structs, queries)
	}

	if err := validate(options, enums, structs, queries); err != nil {
		return nil, err
	}

	return generate(req, options, enums, structs, queries)
}

func validate(options *opts.Options, enums []Enum, structs []Struct, queries []Query) error {
	enumNames := make(map[string]struct{})
	for _, enum := range enums {
		enumNames[enum.Name] = struct{}{}
		enumNames["Null"+enum.Name] = struct{}{}
	}
	structNames := make(map[string]struct{})
	for _, struckt := range structs {
		if _, ok := enumNames[struckt.Name]; ok {
			return fmt.Errorf("struct name conflicts with enum name: %s", struckt.Name)
		}
		structNames[struckt.Name] = struct{}{}
	}
	if !options.EmitExportedQueries {
		return nil
	}
	for _, query := range queries {
		if _, ok := enumNames[query.ConstantName]; ok {
			return fmt.Errorf("query constant name conflicts with enum name: %s", query.ConstantName)
		}
		if _, ok := structNames[query.ConstantName]; ok {
			return fmt.Errorf("query constant name conflicts with struct name: %s", query.ConstantName)
		}
	}
	return nil
}

func generate(req *plugin.GenerateRequest, options *opts.Options, enums []Enum, structs []Struct, queries []Query) (*plugin.GenerateResponse, error) {
	i := &importer{
		Options: options,
		Queries: queries,
		Enums:   enums,
		Structs: structs,
	}

	tctx := tmplCtx{
		EmitInterface:             options.EmitInterface,
		EmitJSONTags:              options.EmitJsonTags,
		JsonTagsIDUppercase:       options.JsonTagsIdUppercase,
		EmitDBTags:                options.EmitDbTags,
		EmitPreparedQueries:       options.EmitPreparedQueries,
		EmitEmptySlices:           options.EmitEmptySlices,
		EmitNilRecords:            options.EmitNilRecords,
		EmitMethodsWithDBArgument: options.EmitMethodsWithDbArgument,
		EmitEnumValidMethod:       options.EmitEnumValidMethod,
		EmitAllEnumValues:         options.EmitAllEnumValues,
		UsesCopyFrom:              usesCopyFrom(queries),
		UsesBatch:                 usesBatch(queries),
		SQLDriver:                 parseDriver(options.SqlPackage),
		Q:                         "`",
		Package:                   options.Package,
		Enums:                     enums,
		Structs:                   structs,
		SqlcVersion:               req.SqlcVersion,
		BuildTags:                 options.BuildTags,
		OmitSqlcVersion:           options.OmitSqlcVersion,
	}

	if tctx.UsesCopyFrom && !tctx.SQLDriver.IsPGX() && options.SqlDriver != opts.SQLDriverGoSQLDriverMySQL {
		return nil, errors.New(":copyfrom is only supported by pgx and github.com/go-sql-driver/mysql")
	}

	if tctx.UsesCopyFrom && options.SqlDriver == opts.SQLDriverGoSQLDriverMySQL {
		if err := checkNoTimesForMySQLCopyFrom(queries); err != nil {
			return nil, err
		}
		tctx.SQLDriver = opts.SQLDriverGoSQLDriverMySQL
	}

	if tctx.UsesBatch && !tctx.SQLDriver.IsPGX() {
		return nil, errors.New(":batch* commands are only supported by pgx")
	}

	funcMap := template.FuncMap{
		"lowerTitle": sdk.LowerTitle,
		"comment":    sdk.DoubleSlashComment,
		"escape":     sdk.EscapeBacktick,
		"imports":    i.Imports,
		"hasImports": i.HasImports,
		"hasPrefix":  strings.HasPrefix,

		// These methods are Go specific, they do not belong in the codegen package
		// (as that is language independent)
		"dbarg":               tctx.codegenDbarg,
		"emitPreparedQueries": tctx.codegenEmitPreparedQueries,
		"queryMethod":         tctx.codegenQueryMethod,
		"queryRetval":         tctx.codegenQueryRetval,
	}

	tmpl := template.Must(
		template.New("table").
			Funcs(funcMap).
			ParseFS(
				templates,
				"templates/*.tmpl",
				"templates/*/*.tmpl",
			),
	)

	output := map[string]string{}

	execute := func(name, packageName, templateName string) error {
		imports := i.Imports(name)
		replacedQueries := replaceConflictedArg(imports, queries)

		var b bytes.Buffer
		w := bufio.NewWriter(&b)
		tctx.SourceName = name
		tctx.GoQueries = replacedQueries
		tctx.Package = packageName
		err := tmpl.ExecuteTemplate(w, templateName, &tctx)
		w.Flush()
		if err != nil {
			return err
		}
		code, err := format.Source(b.Bytes())
		if err != nil {
			fmt.Println(b.String())
			return fmt.Errorf("source error: %w", err)
		}

		if templateName == "queryFile" {
			if options.OutputFilesPrefix != "" {
				name = options.OutputFilesPrefix + name
			}
			if options.OutputFilesSuffix != "" {
				name += options.OutputFilesSuffix
			}
		}

		if !strings.HasSuffix(name, ".go") {
			name += ".go"
		}
		output[name] = string(code)
		return nil
	}

	dbPackageName := options.Package
	dbFileName := "db.go"
	if options.OutputDbPackage != "" {
		dbPackageName = options.OutputDbPackage
	}
	if options.OutputDbFileName != "" {
		dbFileName = options.OutputDbFileName
	}

	modelsPackageName := options.Package
	modelsFileName := "models.go"
	if options.OutputModelsPackage != "" {
		modelsPackageName = options.OutputModelsPackage
	}
	if options.OutputModelsFileName != "" {
		modelsFileName = options.OutputModelsFileName
	}

	querierPackageName := options.Package
	querierFileName := "querier.go"
	if options.OutputQuerierPackage != "" {
		querierPackageName = options.OutputQuerierPackage
	}
	if options.OutputQuerierFileName != "" {
		querierFileName = options.OutputQuerierFileName
	}

	copyfromPackageName := options.Package
	copyfromFileName := "copyfrom.go"
	if options.OutputCopyfromPackage != "" {
		copyfromPackageName = options.OutputCopyfromPackage
	}
	if options.OutputCopyfromFileName != "" {
		copyfromFileName = options.OutputCopyfromFileName
	}

	batchPackageName := options.Package
	batchFileName := "batch.go"
	if options.OutputBatchPackage != "" {
		batchPackageName = options.OutputBatchPackage
	}
	if options.OutputBatchFileName != "" {
		batchFileName = options.OutputBatchFileName
	}

	if err := execute(dbFileName, dbPackageName, "dbFile"); err != nil {
		return nil, err
	}
	if err := execute(modelsFileName, modelsPackageName, "modelsFile"); err != nil {
		return nil, err
	}
	if options.EmitInterface {
		if err := execute(querierFileName, querierPackageName, "interfaceFile"); err != nil {
			return nil, err
		}
	}
	if tctx.UsesCopyFrom {
		if err := execute(copyfromFileName, copyfromPackageName, "copyfromFile"); err != nil {
			return nil, err
		}
	}
	if tctx.UsesBatch {
		if err := execute(batchFileName, batchPackageName, "batchFile"); err != nil {
			return nil, err
		}
	}

	files := map[string]struct{}{}
	for _, gq := range queries {
		files[gq.SourceName] = struct{}{}
	}

	sourcePackageName := options.Package
	if options.OutputFilesPackage != "" {
		sourcePackageName = options.OutputFilesPackage
	}

	for source := range files {
		if err := execute(source, sourcePackageName, "queryFile"); err != nil {
			return nil, err
		}
	}
	resp := plugin.GenerateResponse{}

	for filename, code := range output {
		resp.Files = append(resp.Files, &plugin.File{
			Name:     filename,
			Contents: []byte(code),
		})
	}

	return &resp, nil
}

func usesCopyFrom(queries []Query) bool {
	for _, q := range queries {
		if q.Cmd == metadata.CmdCopyFrom {
			return true
		}
	}
	return false
}

func usesBatch(queries []Query) bool {
	for _, q := range queries {
		for _, cmd := range []string{metadata.CmdBatchExec, metadata.CmdBatchMany, metadata.CmdBatchOne} {
			if q.Cmd == cmd {
				return true
			}
		}
	}
	return false
}

func checkNoTimesForMySQLCopyFrom(queries []Query) error {
	for _, q := range queries {
		if q.Cmd != metadata.CmdCopyFrom {
			continue
		}
		for _, f := range q.Arg.CopyFromMySQLFields() {
			if f.Type == "time.Time" {
				return fmt.Errorf("values with a timezone are not yet supported")
			}
		}
	}
	return nil
}

func filterUnusedStructs(enums []Enum, structs []Struct, queries []Query) ([]Enum, []Struct) {
	keepTypes := make(map[string]struct{})

	for _, query := range queries {
		if !query.Arg.isEmpty() {
			keepTypes[query.Arg.Type()] = struct{}{}
			if query.Arg.IsStruct() {
				for _, field := range query.Arg.Struct.Fields {
					keepTypes[field.Type] = struct{}{}
				}
			}
		}
		if query.hasRetType() {
			keepTypes[query.Ret.Type()] = struct{}{}
			if query.Ret.IsStruct() {
				for _, field := range query.Ret.Struct.Fields {
					keepTypes[field.Type] = struct{}{}
					for _, embedField := range field.EmbedFields {
						keepTypes[embedField.Type] = struct{}{}
					}
				}
			}
		}
	}

	keepTypesNames := make([]string, len(keepTypes))
	i := 0
	for key, _ := range keepTypes {
		// if OutputModelsPackage is different from OutputDbPackage,
		// comparisons between keepTypes and enums and structs become incorrect
		// Therefore package name is stripped from keepTypes
		if strings.Contains(key, ".") {
			keepTypesNames[i] = strings.Split(key, ".")[1]
		} else {
			keepTypesNames[i] = key
		}
		i++
	}

	keepEnums := make([]Enum, 0, len(enums))
	for _, enum := range enums {
		keep := slices.Contains(keepTypesNames, enum.Name)
		keepNull := slices.Contains(keepTypesNames, "Null"+enum.Name)
		if keep || keepNull {
			keepEnums = append(keepEnums, enum)
		}
	}

	keepStructs := make([]Struct, 0, len(structs))
	for _, st := range structs {
		if slices.Contains(keepTypesNames, st.Name) {
			keepStructs = append(keepStructs, st)
		}
	}

	return keepEnums, keepStructs
}
