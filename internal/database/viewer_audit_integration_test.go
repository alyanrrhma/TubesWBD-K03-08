package database

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alyanrrhma/TubesWBD-K02-08/internal/telemetry"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestPostgresViewerReadLogsAndTraceCorrelation(t *testing.T) {
	dsn := os.Getenv("TARGET_DATABASE_URL")
	if dsn == "" {
		t.Skip("TARGET_DATABASE_URL not set")
	}
	parsed, err := url.Parse(dsn)
	if err != nil || !strings.HasSuffix(strings.TrimPrefix(parsed.Path, "/"), "_test") {
		t.Fatalf("TARGET_DATABASE_URL must point to a disposable *_test database")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err = db.PingContext(ctx); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	defer slog.SetDefault(previous)
	const incoming = "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"
	ctx = telemetry.FromHeader(ctx, incoming)
	ctx = WithQueryAuditContext(ctx, 41, 73)
	if _, err = ListSchemas(ctx, db); err != nil {
		t.Fatal(err)
	}

	var started, finished, span map[string]any
	scanner := bufio.NewScanner(bytes.NewReader(output.Bytes()))
	for scanner.Scan() {
		var item map[string]any
		if err = json.Unmarshal(scanner.Bytes(), &item); err != nil {
			t.Fatal(err)
		}
		switch item["msg"] {
		case "sql_query_started":
			started = item
		case "sql_query_finished":
			finished = item
		case "otel_span":
			if item["name"] == "db.viewer.query" {
				span = item
			}
		}
	}
	if err = scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if started == nil || finished == nil {
		t.Fatalf("expected started and finished query logs; got %s", output.String())
	}
	if os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "" && span == nil {
		t.Fatalf("expected console database span log; got %s", output.String())
	}
	for key, want := range map[string]any{"event": "database.sql.read", "actor_id": float64(41), "database_id": float64(73), "query": started["query"], "trace_id": "0123456789abcdef0123456789abcdef", "span_id": started["span_id"]} {
		if started[key] != want {
			t.Errorf("start %s=%v want %v", key, started[key], want)
		}
	}
	for key, want := range map[string]any{"event": "database.sql.read", "actor_id": float64(41), "database_id": float64(73), "query": started["query"], "trace_id": started["trace_id"], "span_id": started["span_id"], "status": "success"} {
		if finished[key] != want {
			t.Errorf("finish %s=%v want %v", key, finished[key], want)
		}
	}
	rowCount, hasRowCount := finished["row_count"].(float64)
	if finished["time"] == nil || finished["level"] != "INFO" || finished["duration_ms"] == nil || !hasRowCount || rowCount < 1 {
		t.Fatalf("required SQL result attributes absent: %+v", finished)
	}
	if span != nil && (span["trace_id"] != started["trace_id"] || span["span_id"] != started["span_id"] || span["parent_span_id"] != "0123456789abcdef" || span["duration_ms"] == nil || span["error"] != nil) {
		t.Fatalf("database span correlation incorrect: %+v", span)
	}
}
