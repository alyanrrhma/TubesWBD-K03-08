package database

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/alyanrrhma/TubesWBD-K02-08/internal/telemetry"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestAuditQueryEmitsSQLContextAndSQLState(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	defer slog.SetDefault(previous)
	parent := telemetry.FromHeader(context.Background(), "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01")
	ctx := WithQueryAuditContext(parent, 41, 73)
	queryCtx, started := startAuditQuery(ctx, "SELECT * FROM public.items WHERE id = $1", []any{9})
	output.Reset()
	auditQuery(queryCtx, "SELECT * FROM public.items WHERE id = $1", []any{9}, started, 2,
		&pgconn.PgError{Code: "42P01", Message: "relation does not exist"})
	var record map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(output.Bytes()), []byte("\n")) {
		var item map[string]any
		if err := json.Unmarshal(line, &item); err != nil {
			t.Fatal(err)
		}
		if item["msg"] == "sql_query_finished" {
			record = item
		}
	}
	for key, want := range map[string]any{"event": "database.sql.read", "actor_id": float64(41), "database_id": float64(73), "status": "error", "sqlstate": "42P01", "row_count": float64(2), "trace_id": telemetry.TraceID(queryCtx), "span_id": telemetry.SpanID(queryCtx)} {
		if record[key] != want {
			t.Errorf("%s=%v, want %v", key, record[key], want)
		}
	}
	if record["time"] == nil || record["level"] != "ERROR" {
		t.Fatalf("missing timestamp or error level: %+v", record)
	}
	if record["query"] != "SELECT * FROM public.items WHERE id = $1" {
		t.Errorf("query=%v", record["query"])
	}
	if record["duration_ms"] == nil {
		t.Fatal("duration_ms missing")
	}
}
