package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alyanrrhma/TubesWBD-K02-08/internal/telemetry"
	"github.com/jackc/pgx/v5/pgconn"
)

type queryAuditKey struct{}
type queryAuditSpanKey struct{}
type queryAuditContext struct{ actorID, databaseID int64 }

// WithQueryAuditContext associates a read-only target query with the HTTP
// actor and registered database without passing those identifiers through SQL.
func WithQueryAuditContext(ctx context.Context, actorID, databaseID int64) context.Context {
	return context.WithValue(ctx, queryAuditKey{}, queryAuditContext{actorID: actorID, databaseID: databaseID})
}

func startAuditQuery(ctx context.Context, query string, parameters []any) (context.Context, time.Time) {
	meta, _ := ctx.Value(queryAuditKey{}).(queryAuditContext)
	queryCtx, endSpan := telemetry.Start(ctx, "db.viewer.query", map[string]any{"database_id": meta.databaseID, "query": telemetry.SafeSQL(query)})
	queryCtx = context.WithValue(queryCtx, queryAuditSpanKey{}, endSpan)
	started := time.Now()
	slog.Default().Info("sql_query_started", "event", "database.sql.read", "actor_id", meta.actorID, "database_id", meta.databaseID, "trace_id", telemetry.TraceID(queryCtx), "span_id", telemetry.SpanID(queryCtx), "query", telemetry.SafeSQL(query), "parameters", telemetry.SafeParams(parameters))
	return queryCtx, started
}

func auditQuery(ctx context.Context, query string, parameters []any, started time.Time, rowCount int, err error) {
	meta, _ := ctx.Value(queryAuditKey{}).(queryAuditContext)
	endSpan, _ := ctx.Value(queryAuditSpanKey{}).(func(error))
	attrs := []any{"event", "database.sql.read", "actor_id", meta.actorID, "database_id", meta.databaseID,
		"trace_id", telemetry.TraceID(ctx), "span_id", telemetry.SpanID(ctx), "query", telemetry.SafeSQL(query), "parameters", telemetry.SafeParams(parameters), "duration_ms", time.Since(started).Milliseconds(), "row_count", rowCount}
	if endSpan != nil {
		endSpan(err)
	}
	if err != nil {
		attrs = append(attrs, "status", "error", "error", err.Error())
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			attrs = append(attrs, "sqlstate", pgErr.Code)
		}
		slog.Default().Error("sql_query_finished", attrs...)
		return
	}
	attrs = append(attrs, "status", "success")
	slog.Default().Info("sql_query_finished", attrs...)
}

const (
	queryTimeout       = 15 * time.Second
	maxIdentifierBytes = 63
)

func validateIdentifier(kind, s string) error {
	switch {
	case s == "":
		return fmt.Errorf("%w: %s must not be empty", ErrInvalidArgument, kind)
	case len(s) > maxIdentifierBytes:
		return fmt.Errorf("%w: %s is longer than %d bytes", ErrInvalidArgument, kind, maxIdentifierBytes)
	case strings.ContainsRune(s, 0):
		return fmt.Errorf("%w: %s contains a NUL character", ErrInvalidArgument, kind)
	case !utf8.ValidString(s):
		return fmt.Errorf("%w: %s is not valid UTF-8", ErrInvalidArgument, kind)
	}
	return nil
}

// wrapQueryError menerjemahkan error PostgreSQL yang penting menjadi sentinel.
func wrapQueryError(op string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %s", ErrQueryTimeout, op)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "42501": // insufficient_privilege
			return fmt.Errorf("%w: %s", ErrTargetPermissionDenied, op)
		case "42P01": // undefined_table
			return fmt.Errorf("%w: %s", ErrTableNotFound, op)
		case "57014": // query_canceled: pembatalan dari server saat batas waktu habis
			return fmt.Errorf("%w: %s", ErrQueryTimeout, op)
		case "25006": // read_only_sql_transaction
			return fmt.Errorf("%w: %s", ErrWriteRejected, op)
		}
	}
	return fmt.Errorf("%s: %w", op, err)
}

type Table struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
}
type Column struct {
	Name       string `json:"name"`
	DataType   string `json:"data_type"`
	IsNullable bool   `json:"is_nullable"`
	Position   int    `json:"position"`
}
type Schema struct {
	Name string `json:"name"`
}
type Constraint struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Definition string `json:"definition"`
}
type Index struct {
	Name       string `json:"name"`
	Definition string `json:"definition"`
	IsUnique   bool   `json:"is_unique"`
	IsPrimary  bool   `json:"is_primary"`
}
type TableData struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	TotalRows int      `json:"total_rows"`
	Page      int      `json:"page"`
	PageSize  int      `json:"page_size"`
}

// ListTables mengambil daftar tabel biasa dalam sebuah schema.
func ListTables(ctx context.Context, db *sql.DB, schema string) (tables []Table, retErr error) {
	if db == nil {
		return nil, fmt.Errorf("database connection is nil")
	}
	if err := validateIdentifier("schema", schema); err != nil {
		return nil, err
	}

	const query = `
		SELECT table_schema, table_name
		FROM information_schema.tables
		WHERE table_schema = $1
		  AND table_type = 'BASE TABLE'
		ORDER BY table_name
	`
	queryCtx, started := startAuditQuery(ctx, query, []any{schema})
	rows, err := db.QueryContext(ctx, query, schema)
	if err != nil {
		retErr = wrapQueryError("list tables", err)
		auditQuery(queryCtx, query, []any{schema}, started, 0, retErr)
		return nil, retErr
	}
	defer rows.Close()
	defer func() { auditQuery(queryCtx, query, []any{schema}, started, len(tables), retErr) }()

	tables = make([]Table, 0)

	for rows.Next() {
		var table Table
		if err := rows.Scan(&table.Schema, &table.Name); err != nil {
			return nil, wrapQueryError("scan table", err)
		}
		tables = append(tables, table)
	}

	if err := rows.Err(); err != nil {
		return nil, wrapQueryError("iterate tables", err)
	}

	return tables, nil
}

// ListColumns mengambil daftar kolom dari tabel tertentu.
func ListColumns(ctx context.Context, db *sql.DB, schema string, table string) (columns []Column, retErr error) {
	if db == nil {
		return nil, fmt.Errorf("database connection is nil")
	}
	if err := validateIdentifier("schema", schema); err != nil {
		return nil, err
	}
	if err := validateIdentifier("table", table); err != nil {
		return nil, err
	}

	const query = `
		SELECT
			column_name,
			data_type,
			is_nullable,
			ordinal_position
		FROM information_schema.columns
		WHERE table_schema = $1
		  AND table_name = $2
		ORDER BY ordinal_position
	`
	queryCtx, started := startAuditQuery(ctx, query, []any{schema, table})
	rows, err := db.QueryContext(ctx, query, schema, table)
	if err != nil {
		retErr = wrapQueryError("list columns", err)
		auditQuery(queryCtx, query, []any{schema, table}, started, 0, retErr)
		return nil, retErr
	}
	defer rows.Close()
	defer func() { auditQuery(queryCtx, query, []any{schema, table}, started, len(columns), retErr) }()

	columns = make([]Column, 0)

	for rows.Next() {
		var column Column
		var nullable string

		if err := rows.Scan(
			&column.Name,
			&column.DataType,
			&nullable,
			&column.Position,
		); err != nil {
			return nil, wrapQueryError("scan column", err)
		}

		column.IsNullable = nullable == "YES"
		columns = append(columns, column)
	}

	if err := rows.Err(); err != nil {
		return nil, wrapQueryError("iterate columns", err)
	}

	return columns, nil
}

// ListSchemas mengambil daftar schema yang dapat diakses
// pada database target.
func ListSchemas(ctx context.Context, db *sql.DB) (schemas []Schema, retErr error) {
	if db == nil {
		return nil, fmt.Errorf("database connection is nil")
	}

	const query = `
		SELECT schema_name
		FROM information_schema.schemata
		WHERE schema_name NOT IN (
			'information_schema',
			'pg_catalog'
		)
		AND schema_name NOT LIKE 'pg_toast%%'
		AND schema_name NOT LIKE 'pg_temp_%%'
		ORDER BY schema_name
	`
	queryCtx, started := startAuditQuery(ctx, query, nil)
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		retErr = wrapQueryError("list schemas", err)
		auditQuery(queryCtx, query, nil, started, 0, retErr)
		return nil, retErr
	}
	defer rows.Close()
	defer func() { auditQuery(queryCtx, query, nil, started, len(schemas), retErr) }()

	schemas = make([]Schema, 0)

	for rows.Next() {
		var schema Schema
		if err := rows.Scan(&schema.Name); err != nil {
			return nil, wrapQueryError("scan schema", err)
		}
		schemas = append(schemas, schema)
	}

	if err := rows.Err(); err != nil {
		return nil, wrapQueryError("iterate schemas", err)
	}

	return schemas, nil
}

// ListConstraints mengambil constraint dari tabel tertentu.
func ListConstraints(ctx context.Context, db *sql.DB, schema string, table string) (constraints []Constraint, retErr error) {
	if db == nil {
		return nil, fmt.Errorf("database connection is nil")
	}
	if err := validateIdentifier("schema", schema); err != nil {
		return nil, err
	}
	if err := validateIdentifier("table", table); err != nil {
		return nil, err
	}

	const query = `
        SELECT
            c.conname,
            CASE c.contype
                WHEN 'p' THEN 'PRIMARY KEY'
                WHEN 'u' THEN 'UNIQUE'
                WHEN 'f' THEN 'FOREIGN KEY'
                WHEN 'c' THEN 'CHECK'
                WHEN 'x' THEN 'EXCLUDE'
                ELSE 'OTHER'
            END AS constraint_type,
            pg_get_constraintdef(c.oid, true) AS definition
        FROM pg_catalog.pg_constraint c
        JOIN pg_catalog.pg_class t
            ON t.oid = c.conrelid
        JOIN pg_catalog.pg_namespace n
            ON n.oid = t.relnamespace
        WHERE n.nspname = $1
          AND t.relname = $2
        ORDER BY c.conname
    `

	queryCtx, started := startAuditQuery(ctx, query, []any{schema, table})
	rows, err := db.QueryContext(ctx, query, schema, table)
	if err != nil {
		retErr = wrapQueryError("list table constraints", err)
		auditQuery(queryCtx, query, []any{schema, table}, started, 0, retErr)
		return nil, retErr
	}
	defer rows.Close()
	defer func() { auditQuery(queryCtx, query, []any{schema, table}, started, len(constraints), retErr) }()

	constraints = make([]Constraint, 0)

	for rows.Next() {
		var c Constraint
		if err := rows.Scan(&c.Name, &c.Type, &c.Definition); err != nil {
			return nil, wrapQueryError("scan table constraint", err)
		}
		constraints = append(constraints, c)
	}

	if err := rows.Err(); err != nil {
		return nil, wrapQueryError("iterate table constraints", err)
	}

	return constraints, nil
}

// ListIndexes mengambil index dari tabel tertentu.
func ListIndexes(ctx context.Context, db *sql.DB, schema string, table string) (indexes []Index, retErr error) {
	if db == nil {
		return nil, fmt.Errorf("database connection is nil")
	}
	if err := validateIdentifier("schema", schema); err != nil {
		return nil, err
	}
	if err := validateIdentifier("table", table); err != nil {
		return nil, err
	}

	const query = `
		SELECT
			idx.relname,
			pg_get_indexdef(i.indexrelid),
			i.indisunique,
			i.indisprimary
		FROM pg_catalog.pg_index i
		JOIN pg_catalog.pg_class tbl
			ON tbl.oid = i.indrelid
		JOIN pg_catalog.pg_namespace ns
			ON ns.oid = tbl.relnamespace
		JOIN pg_catalog.pg_class idx
			ON idx.oid = i.indexrelid
		WHERE ns.nspname = $1
			AND tbl.relname = $2
		ORDER BY idx.relname
	`
	queryCtx, started := startAuditQuery(ctx, query, []any{schema, table})
	rows, err := db.QueryContext(ctx, query, schema, table)
	if err != nil {
		retErr = wrapQueryError("list indexes", err)
		auditQuery(queryCtx, query, []any{schema, table}, started, 0, retErr)
		return nil, retErr
	}
	defer rows.Close()
	defer func() { auditQuery(queryCtx, query, []any{schema, table}, started, len(indexes), retErr) }()

	indexes = make([]Index, 0)

	for rows.Next() {
		var idx Index
		if err := rows.Scan(
			&idx.Name,
			&idx.Definition,
			&idx.IsUnique,
			&idx.IsPrimary,
		); err != nil {
			return nil, wrapQueryError("scan index", err)
		}
		indexes = append(indexes, idx)
	}

	if err := rows.Err(); err != nil {
		return nil, wrapQueryError("iterate indexes", err)
	}

	return indexes, nil
}

// ReadTableData membaca isi tabel dengan pagination. Page dimulai dari 1.
func ReadTableData(ctx context.Context, db *sql.DB, schema string, table string, page int, pageSize int) (result TableData, retErr error) {
	if db == nil {
		return TableData{}, fmt.Errorf("database connection is nil")
	}
	if err := validateIdentifier("schema", schema); err != nil {
		return TableData{}, err
	}
	if err := validateIdentifier("table", table); err != nil {
		return TableData{}, err
	}
	if page < 1 {
		return TableData{}, fmt.Errorf("%w: page must be at least 1", ErrInvalidArgument)
	}
	if pageSize < 1 || pageSize > 100 {
		return TableData{}, fmt.Errorf("%w: page size must be between 1 and 100", ErrInvalidArgument)
	}

	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	// Memastikan tabel tersedia dan memperoleh daftar kolomnya.
	metadata, err := ListColumns(ctx, db, schema, table)
	if err != nil {
		return TableData{}, err
	}
	if len(metadata) == 0 {
		return TableData{}, fmt.Errorf("%w: %q.%q", ErrTableNotFound, schema, table)
	}

	result = TableData{
		Columns:  make([]string, 0, len(metadata)),
		Rows:     make([][]any, 0),
		Page:     page,
		PageSize: pageSize,
	}

	for _, column := range metadata {
		result.Columns = append(result.Columns, column.Name)
	}

	qualifiedTable := quoteIdentifier(schema) + "." + quoteIdentifier(table)

	tx, err := db.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return TableData{}, wrapQueryError("begin read-only transaction", err)
	}
	defer tx.Rollback()

	// Menghitung total baris untuk informasi pagination.
	countQuery := "SELECT COUNT(*) FROM " + qualifiedTable
	countQueryCtx, countStarted := startAuditQuery(ctx, countQuery, nil)
	countErr := tx.QueryRowContext(ctx, countQuery).Scan(&result.TotalRows)
	countRows := 0
	if countErr == nil {
		countRows = 1
	}
	auditQuery(countQueryCtx, countQuery, nil, countStarted, countRows, countErr)
	if countErr != nil {
		return TableData{}, wrapQueryError("count table rows", countErr)
	}
	pages := 1
	if result.TotalRows > 0 {
		pages += (result.TotalRows - 1) / pageSize
	}
	if page > pages {
		return result, nil
	}

	// Mengambil kolom primary key untuk menentukan urutan baris.
	primaryKeys, err := primaryKeyColumns(ctx, tx, schema, table)
	if err != nil {
		return TableData{}, err
	}

	query := "SELECT * FROM " + qualifiedTable

	if len(primaryKeys) > 0 {
		// Jika ada primary key, gunakan sebagai urutan baris.
		orderColumns := make([]string, 0, len(primaryKeys))

		for _, name := range primaryKeys {
			orderColumns = append(orderColumns, quoteIdentifier(name))
		}

		query += " ORDER BY " + strings.Join(orderColumns, ", ")
	} else {
		// Jika tidak ada primary key, ctid menjadi urutan sementara.
		// ctid bukan identitas permanen dan bisa berubah.
		query += " ORDER BY ctid"
	}

	offset := (page - 1) * pageSize
	query += " LIMIT $1 OFFSET $2"

	queryCtx, queryStarted := startAuditQuery(ctx, query, []any{pageSize, offset})
	rows, err := tx.QueryContext(ctx, query, pageSize, offset)
	if err != nil {
		wrapped := wrapQueryError("read table data", err)
		auditQuery(queryCtx, query, []any{pageSize, offset}, queryStarted, 0, wrapped)
		return TableData{}, wrapped
	}
	defer rows.Close()
	defer func() { auditQuery(queryCtx, query, []any{pageSize, offset}, queryStarted, len(result.Rows), retErr) }()

	for rows.Next() {
		values := make([]any, len(result.Columns))
		destinations := make([]any, len(values))

		for i := range values {
			destinations[i] = &values[i]
		}

		if err := rows.Scan(destinations...); err != nil {
			return TableData{}, wrapQueryError("scan table row", err)
		}

		result.Rows = append(result.Rows, values)
	}

	if err := rows.Err(); err != nil {
		return TableData{}, wrapQueryError("iterate table data", err)
	}

	return result, nil
}

// quoteIdentifier mengamankan nama schema, tabel, dan kolom yang digunakan sebagai identifier SQL PostgreSQL.
func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// Menggunakan katalog PostgreSQL untuk mendapatkan kolom primary key, termasuk jika primary key terdiri dari beberapa kolom.
func primaryKeyColumns(ctx context.Context, tx *sql.Tx, schema string, table string) (columns []string, retErr error) {
	const query = `
        SELECT a.attname
        FROM pg_catalog.pg_index i
        JOIN pg_catalog.pg_class t
            ON t.oid = i.indrelid
        JOIN pg_catalog.pg_namespace n
            ON n.oid = t.relnamespace
        JOIN LATERAL
            unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord)
            ON true
        JOIN pg_catalog.pg_attribute a
            ON a.attrelid = t.oid
           AND a.attnum = k.attnum
        WHERE i.indisprimary
          AND n.nspname = $1
          AND t.relname = $2
        ORDER BY k.ord
    `

	queryCtx, started := startAuditQuery(ctx, query, []any{schema, table})
	rows, err := tx.QueryContext(ctx, query, schema, table)
	if err != nil {
		retErr = wrapQueryError("find primary key columns", err)
		auditQuery(queryCtx, query, []any{schema, table}, started, 0, retErr)
		return nil, retErr
	}
	defer rows.Close()
	defer func() { auditQuery(queryCtx, query, []any{schema, table}, started, len(columns), retErr) }()

	columns = make([]string, 0)

	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			return nil, wrapQueryError("scan primary key column", err)
		}
		columns = append(columns, column)
	}

	if err := rows.Err(); err != nil {
		return nil, wrapQueryError("find primary key columns", err)
	}

	return columns, nil
}
