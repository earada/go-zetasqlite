package internal

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"
	"github.com/goccy/go-zetasql/types"
)

var (
	createCatalogTableQuery = `
CREATE TABLE IF NOT EXISTS zetasqlite_catalog(
  name STRING NOT NULL PRIMARY KEY,
  kind STRING NOT NULL,
  spec STRING NOT NULL,
  updatedAt TIMESTAMP NOT NULL,
  createdAt TIMESTAMP NOT NULL
)
`
	upsertCatalogQuery = `
INSERT INTO zetasqlite_catalog (
  name,
  kind,
  spec,
  updatedAt,
  createdAt
) VALUES (
  @name,
  @kind,
  @spec,
  @updatedAt,
  @createdAt
) ON CONFLICT(name) DO UPDATE SET
  spec = @spec,
  updatedAt = @updatedAt
`
	deleteCatalogQuery = `
DELETE FROM zetasqlite_catalog WHERE name = @name
`
)

type CatalogSpecKind string

const (
	TableSpecKind      CatalogSpecKind = "table"
	FunctionSpecKind   CatalogSpecKind = "function"
	defaultCatalogName                 = "zetasqlite"
)

type Catalog struct {
	db               *sql.DB
	lastSyncedAt     time.Time
	mu               sync.Mutex
	tables           []*TableSpec
	functions        []*FunctionSpec
	defaultCatalog   *types.SimpleCatalog
	pathToCatalogMap map[string]*types.SimpleCatalog
	tableMap         map[string]*TableSpec
	funcMap          map[string]*FunctionSpec
}

func newSimpleCatalog(name string) *types.SimpleCatalog {
	catalog := types.NewSimpleCatalog(name)
	catalog.AddZetaSQLBuiltinFunctions(nil)
	return catalog
}

func NewCatalog(db *sql.DB) *Catalog {
	return &Catalog{
		db:               db,
		defaultCatalog:   newSimpleCatalog(defaultCatalogName),
		pathToCatalogMap: map[string]*types.SimpleCatalog{},
		tableMap:         map[string]*TableSpec{},
		funcMap:          map[string]*FunctionSpec{},
	}
}

func (c *Catalog) pathToCatalogMapKey(path []string) string {
	return strings.Join(path, "_")
}

func (c *Catalog) getCatalog(path []string) *types.SimpleCatalog {
	cat, exists := c.pathToCatalogMap[c.pathToCatalogMapKey(path)]
	if !exists {
		return c.defaultCatalog
	}
	return cat
}

func (c *Catalog) getFunctions(path []string) []*FunctionSpec {
	if len(path) == 0 {
		return c.functions
	}
	key := c.pathToCatalogMapKey(path)
	specs := make([]*FunctionSpec, 0, len(c.functions))
	for _, fn := range c.functions {
		if len(fn.NamePath) == 1 {
			// function name only
			specs = append(specs, fn)
			continue
		}
		pathPrefixKey := c.pathToCatalogMapKey(c.trimmedLastPath(fn.NamePath))
		if strings.Contains(pathPrefixKey, key) {
			specs = append(specs, fn)
		}
	}
	return specs
}

func (c *Catalog) Sync(ctx context.Context, conn *Conn) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.createCatalogTablesIfNotExists(ctx, conn); err != nil {
		return fmt.Errorf("failed to create catalog tables: %w", err)
	}
	now := time.Now()
	rows, err := conn.QueryContext(
		ctx,
		`SELECT name, kind, spec FROM zetasqlite_catalog WHERE updatedAt >= @lastUpdatedAt`,
		c.lastSyncedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to query load catalog: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			name string
			kind CatalogSpecKind
			spec string
		)
		if err := rows.Scan(&name, &kind, &spec); err != nil {
			return fmt.Errorf("failed to scan catalog values: %w", err)
		}
		switch kind {
		case TableSpecKind:
			if err := c.loadTableSpec(spec); err != nil {
				return fmt.Errorf("failed to load table spec: %w", err)
			}
		case FunctionSpecKind:
			if err := c.loadFunctionSpec(spec); err != nil {
				return fmt.Errorf("failed to load function spec: %w", err)
			}
		default:
			return fmt.Errorf("unknown catalog spec kind %s", kind)
		}
	}
	c.lastSyncedAt = now
	return nil
}

func (c *Catalog) AddNewTableSpec(ctx context.Context, conn *Conn, spec *TableSpec) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.addTableSpec(spec); err != nil {
		return err
	}
	if !spec.IsTemp {
		if err := c.saveTableSpec(ctx, conn, spec); err != nil {
			return err
		}
	}
	return nil
}

func (c *Catalog) AddNewFunctionSpec(ctx context.Context, conn *Conn, spec *FunctionSpec) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.addFunctionSpec(spec); err != nil {
		return err
	}
	if !spec.IsTemp {
		if err := c.saveFunctionSpec(ctx, conn, spec); err != nil {
			return err
		}
	}
	return nil
}

func (c *Catalog) DeleteTableSpec(ctx context.Context, conn *Conn, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.deleteTableSpecByName(name); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, deleteCatalogQuery, sql.Named("name", name)); err != nil {
		return err
	}
	return nil
}

func (c *Catalog) DeleteFunctionSpec(ctx context.Context, conn *Conn, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.deleteFunctionSpecByName(name); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, deleteCatalogQuery, sql.Named("name", name)); err != nil {
		return err
	}
	return nil
}

func (c *Catalog) deleteTableSpecByName(name string) error {
	spec, exists := c.tableMap[name]
	if !exists {
		return fmt.Errorf("failed to find table spec from map by %s", name)
	}
	tables := make([]*TableSpec, 0, len(c.tables))
	specName := strings.Join(spec.NamePath, "_")
	for _, table := range c.tables {
		if specName == strings.Join(table.NamePath, "_") {
			continue
		}
		tables = append(tables, table)
	}

	c.defaultCatalog = newSimpleCatalog(defaultCatalogName)
	c.pathToCatalogMap = map[string]*types.SimpleCatalog{}
	c.tables = []*TableSpec{}
	c.tableMap = map[string]*TableSpec{}
	for _, spec := range tables {
		if err := c.addTableSpec(spec); err != nil {
			return err
		}
	}
	return nil
}

func (c *Catalog) deleteFunctionSpecByName(name string) error {
	spec, exists := c.funcMap[name]
	if !exists {
		return fmt.Errorf("failed to find function spec from map by %s", name)
	}
	functions := make([]*FunctionSpec, 0, len(c.functions))
	specName := strings.Join(spec.NamePath, "_")
	for _, function := range c.functions {
		if specName == strings.Join(function.NamePath, "_") {
			continue
		}
		functions = append(functions, function)
	}

	c.defaultCatalog = newSimpleCatalog(defaultCatalogName)
	c.pathToCatalogMap = map[string]*types.SimpleCatalog{}
	c.functions = []*FunctionSpec{}
	c.funcMap = map[string]*FunctionSpec{}
	for _, spec := range c.functions {
		if err := c.addFunctionSpec(spec); err != nil {
			return err
		}
	}
	return nil
}

func (c *Catalog) saveTableSpec(ctx context.Context, conn *Conn, spec *TableSpec) error {
	encoded, err := json.Marshal(spec)
	if err != nil {
		return fmt.Errorf("failed to encode table spec: %w", err)
	}
	now := time.Now()
	if _, err := conn.ExecContext(
		ctx,
		upsertCatalogQuery,
		sql.Named("name", spec.TableName()),
		sql.Named("kind", string(TableSpecKind)),
		sql.Named("spec", string(encoded)),
		sql.Named("updatedAt", now),
		sql.Named("createdAt", now),
	); err != nil {
		return fmt.Errorf("failed to save a new table spec: %w", err)
	}
	return nil
}

func (c *Catalog) saveFunctionSpec(ctx context.Context, conn *Conn, spec *FunctionSpec) error {
	encoded, err := json.Marshal(spec)
	if err != nil {
		return fmt.Errorf("failed to encode function spec: %w", err)
	}
	now := time.Now()
	if _, err := conn.ExecContext(
		ctx,
		upsertCatalogQuery,
		sql.Named("name", spec.FuncName()),
		sql.Named("kind", string(FunctionSpecKind)),
		sql.Named("spec", string(encoded)),
		sql.Named("updatedAt", now),
		sql.Named("createdAt", now),
	); err != nil {
		return fmt.Errorf("failed to save a new function spec: %w", err)
	}
	return nil
}

func (c *Catalog) createCatalogTablesIfNotExists(ctx context.Context, conn *Conn) error {
	if _, err := conn.ExecContext(ctx, createCatalogTableQuery); err != nil {
		return fmt.Errorf("failed to create catalog table: %w", err)
	}
	return nil
}

func (c *Catalog) loadTableSpec(spec string) error {
	var v TableSpec
	if err := json.Unmarshal([]byte(spec), &v); err != nil {
		return fmt.Errorf("failed to decode table spec: %w", err)
	}
	if err := c.addTableSpec(&v); err != nil {
		return fmt.Errorf("failed to add table spec to catalog: %w", err)
	}
	return nil
}

func (c *Catalog) loadFunctionSpec(spec string) error {
	var v FunctionSpec
	if err := json.Unmarshal([]byte(spec), &v); err != nil {
		return fmt.Errorf("failed to decode function spec: %w", err)
	}
	if err := c.addFunctionSpec(&v); err != nil {
		return fmt.Errorf("failed to add function spec to catalog: %w", err)
	}
	return nil
}

func (c *Catalog) trimmedLastPath(path []string) []string {
	if len(path) == 0 {
		return path
	}
	return path[:len(path)-1]
}

func (c *Catalog) addFunctionSpec(spec *FunctionSpec) error {
	funcName := spec.FuncName()
	if _, exists := c.funcMap[funcName]; exists {
		c.funcMap[funcName] = spec // update current spec
		return nil
	}
	c.functions = append(c.functions, spec)
	c.funcMap[funcName] = spec
	catalogMapKey := c.pathToCatalogMapKey(c.trimmedLastPath(spec.NamePath))
	if catalogMapKey != "" {
		cat, exists := c.pathToCatalogMap[catalogMapKey]
		if !exists {
			cat = newSimpleCatalog(defaultCatalogName)
			c.pathToCatalogMap[catalogMapKey] = cat
		}
		if err := c.addFunctionSpecRecursive(cat, spec); err != nil {
			return err
		}
	}
	if err := c.addFunctionSpecRecursive(c.defaultCatalog, spec); err != nil {
		return err
	}
	return nil
}

func (c *Catalog) addTableSpec(spec *TableSpec) error {
	tableName := spec.TableName()
	if _, exists := c.tableMap[tableName]; exists {
		c.tableMap[tableName] = spec // update current spec
		return nil
	}
	c.tables = append(c.tables, spec)
	c.tableMap[tableName] = spec
	catalogMapKey := c.pathToCatalogMapKey(c.trimmedLastPath(spec.NamePath))
	if catalogMapKey != "" {
		cat, exists := c.pathToCatalogMap[catalogMapKey]
		if !exists {
			cat = newSimpleCatalog(defaultCatalogName)
			c.pathToCatalogMap[catalogMapKey] = cat
		}
		if err := c.addTableSpecRecursive(cat, spec); err != nil {
			return err
		}
	}
	if err := c.addTableSpecRecursive(c.defaultCatalog, spec); err != nil {
		return err
	}
	return nil
}

func (c *Catalog) addTableSpecRecursive(cat *types.SimpleCatalog, spec *TableSpec) error {
	if len(spec.NamePath) > 1 {
		subCatalogName := spec.NamePath[0]
		subCatalog := newSimpleCatalog(subCatalogName)
		if !c.existsCatalog(cat, subCatalogName) {
			cat.AddCatalog(subCatalog)
		}
		fullTableName := strings.Join(spec.NamePath, ".")
		if !c.existsTable(cat, fullTableName) {
			table, err := c.createSimpleTable(fullTableName, spec)
			if err != nil {
				return err
			}
			cat.AddTable(table)
		}
		newNamePath := spec.NamePath[1:]
		// add sub catalog to root catalog
		if err := c.addTableSpecRecursive(cat, c.copyTableSpec(spec, newNamePath)); err != nil {
			return fmt.Errorf("failed to add table spec to root catalog: %w", err)
		}
		// add sub catalog to parent catalog
		if err := c.addTableSpecRecursive(subCatalog, c.copyTableSpec(spec, newNamePath)); err != nil {
			return fmt.Errorf("failed to add table spec to parent catalog: %w", err)
		}
		return nil
	}
	if len(spec.NamePath) == 0 {
		return fmt.Errorf("table name is not found")
	}

	tableName := spec.NamePath[0]
	if c.existsTable(cat, tableName) {
		return nil
	}
	table, err := c.createSimpleTable(tableName, spec)
	if err != nil {
		return err
	}
	cat.AddTable(table)
	return nil
}

func (c *Catalog) createSimpleTable(tableName string, spec *TableSpec) (*types.SimpleTable, error) {
	columns := []types.Column{}
	for _, column := range spec.Columns {
		typ, err := column.Type.ToZetaSQLType()
		if err != nil {
			return nil, err
		}
		columns = append(columns, types.NewSimpleColumn(
			tableName, column.Name, typ,
		))
	}
	return types.NewSimpleTable(tableName, columns), nil
}

func (c *Catalog) addFunctionSpecRecursive(cat *types.SimpleCatalog, spec *FunctionSpec) error {
	if len(spec.NamePath) > 1 {
		subCatalogName := spec.NamePath[0]
		subCatalog := newSimpleCatalog(subCatalogName)
		if !c.existsCatalog(cat, subCatalogName) {
			cat.AddCatalog(subCatalog)
		}
		newNamePath := spec.NamePath[1:]
		// add sub catalog to root catalog
		if err := c.addFunctionSpecRecursive(cat, c.copyFunctionSpec(spec, newNamePath)); err != nil {
			return fmt.Errorf("failed to add function spec to root catalog: %w", err)
		}
		// add sub catalog to parent catalog
		if err := c.addFunctionSpecRecursive(subCatalog, c.copyFunctionSpec(spec, newNamePath)); err != nil {
			return fmt.Errorf("failed to add function spec to parent catalog: %w", err)
		}
		return nil
	}
	if len(spec.NamePath) == 0 {
		return fmt.Errorf("function name is not found")
	}

	funcName := spec.NamePath[0]
	if c.existsFunction(cat, funcName) {
		return nil
	}
	argTypes := []*types.FunctionArgumentType{}
	for _, arg := range spec.Args {
		t, err := arg.Type.ToZetaSQLType()
		if err != nil {
			return err
		}
		argTypes = append(argTypes, types.NewFunctionArgumentType(arg.Name, t))
	}
	retType, err := spec.Return.ToZetaSQLType()
	if err != nil {
		return err
	}
	returnType := types.NewFunctionArgumentType(spec.Return.Name, retType)
	sig := types.NewFunctionSignature(returnType, argTypes)
	newFunc := types.NewFunction([]string{funcName}, "", types.ScalarMode, []*types.FunctionSignature{sig})
	cat.AddFunction(newFunc)
	return nil
}

func (c *Catalog) existsCatalog(cat *types.SimpleCatalog, name string) bool {
	foundCatalog, _ := cat.Catalog(name)
	return !c.isNilCatalog(foundCatalog)
}

func (c *Catalog) existsTable(cat *types.SimpleCatalog, name string) bool {
	foundTable, _ := cat.FindTable([]string{name})
	return !c.isNilTable(foundTable)
}

func (c *Catalog) existsFunction(cat *types.SimpleCatalog, name string) bool {
	foundFunc, _ := cat.FindFunction([]string{name})
	return foundFunc != nil
}

func (c *Catalog) isNilCatalog(cat types.Catalog) bool {
	v := reflect.ValueOf(cat)
	if !v.IsValid() {
		return true
	}
	return v.IsNil()
}

func (c *Catalog) isNilTable(t types.Table) bool {
	v := reflect.ValueOf(t)
	if !v.IsValid() {
		return true
	}
	return v.IsNil()
}

func (c *Catalog) copyTableSpec(spec *TableSpec, newNamePath []string) *TableSpec {
	return &TableSpec{
		NamePath:   newNamePath,
		Columns:    spec.Columns,
		CreateMode: spec.CreateMode,
	}
}

func (c *Catalog) copyFunctionSpec(spec *FunctionSpec, newNamePath []string) *FunctionSpec {
	return &FunctionSpec{
		NamePath: newNamePath,
		Language: spec.Language,
		Args:     spec.Args,
		Return:   spec.Return,
		Code:     spec.Code,
		Body:     spec.Body,
	}
}
