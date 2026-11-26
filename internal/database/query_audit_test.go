package database

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestAuditQueryEmitsSQLContextAndSQLState(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	defer slog.SetDefault(previous)
	ctx := WithQueryAuditContext(context.Background(), 41, 73)
	auditQuery(ctx, "SELECT * FROM public.items WHERE id = $1", []any{9}, time.Now(), 2,
		&pgconn.PgError{Code: "42P01", Message: "relation does not exist"})
	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &record); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{"event": "database.sql.read", "actor_id": float64(41), "database_id": float64(73), "status": "error", "sqlstate": "42P01", "row_count": float64(2)} {
		if record[key] != want {
			t.Errorf("%s=%v, want %v", key, record[key], want)
		}
	}
	if record["query"] != "SELECT * FROM public.items WHERE id = $1" {
		t.Errorf("query=%v", record["query"])
	}
	if record["duration_ms"] == nil {
		t.Fatal("duration_ms missing")
	}
}
