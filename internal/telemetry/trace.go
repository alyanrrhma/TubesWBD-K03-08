// Package telemetry implements W3C Trace Context propagation and OTLP/HTTP JSON
// span export without introducing a new runtime dependency. The OTLP endpoint
// is optional; when unset, completed spans are emitted as structured JSON logs.
package telemetry

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

type traceContextKey struct{}
type traceState struct{ TraceID, SpanID, Flags string }

var traceparentPattern = regexp.MustCompile(`^00-([0-9a-f]{32})-([0-9a-f]{16})-([0-9a-f]{2})$`)

func FromHeader(ctx context.Context, header string) context.Context {
	m := traceparentPattern.FindStringSubmatch(strings.ToLower(strings.TrimSpace(header)))
	if m == nil || m[1] == strings.Repeat("0", 32) || m[2] == strings.Repeat("0", 16) {
		return ctx
	}
	return context.WithValue(ctx, traceContextKey{}, traceState{m[1], m[2], m[3]})
}
func Traceparent(ctx context.Context) string {
	state, ok := ctx.Value(traceContextKey{}).(traceState)
	if !ok {
		return ""
	}
	return fmt.Sprintf("00-%s-%s-%s", state.TraceID, state.SpanID, state.Flags)
}
func TraceID(ctx context.Context) string {
	state, _ := ctx.Value(traceContextKey{}).(traceState)
	return state.TraceID
}
func SpanID(ctx context.Context) string {
	state, _ := ctx.Value(traceContextKey{}).(traceState)
	return state.SpanID
}
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Start creates a child span. End must be called with the operation error.
// The exported spans follow the OpenTelemetry OTLP/HTTP JSON wire format.
func Start(ctx context.Context, name string, attributes map[string]any) (context.Context, func(error)) {
	parent, _ := ctx.Value(traceContextKey{}).(traceState)
	traceID := parent.TraceID
	if traceID == "" {
		traceID = randomHex(16)
	}
	spanID := randomHex(8)
	state := traceState{traceID, spanID, "01"}
	next := context.WithValue(ctx, traceContextKey{}, state)
	started := time.Now()
	return next, func(err error) {
		ended := time.Now()
		values := make([]map[string]any, 0, len(attributes))
		for k, v := range attributes {
			values = append(values, map[string]any{"key": k, "value": map[string]any{"stringValue": fmt.Sprint(v)}})
		}
		status := map[string]any{"code": 1}
		if err != nil {
			status = map[string]any{"code": 2, "message": err.Error()}
		}
		span := map[string]any{"traceId": traceID, "spanId": spanID, "name": name, "kind": 1, "startTimeUnixNano": fmt.Sprint(started.UnixNano()), "endTimeUnixNano": fmt.Sprint(ended.UnixNano()), "attributes": values, "status": status}
		if parent.SpanID != "" {
			span["parentSpanId"] = parent.SpanID
		}
		record := map[string]any{
			"resourceSpans": []any{
				map[string]any{
					"resource":   map[string]any{"attributes": []any{map[string]any{"key": "service.name", "value": map[string]any{"stringValue": "dbmp"}}}},
					"scopeSpans": []any{map[string]any{"scope": map[string]any{"name": "dbmp.telemetry"}, "spans": []any{span}}},
				},
			},
		}
		data, _ := json.Marshal(record)
		endpoint := os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")
		if endpoint == "" {
			slog.Info("otel_span", "trace_id", traceID, "span_id", spanID, "parent_span_id", parent.SpanID, "name", name, "duration_ms", ended.Sub(started).Milliseconds(), "error", err)
			return
		}
		// Export asynchronously so collector downtime cannot block a migration commit.
		go func() {
			c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			req, reqErr := http.NewRequestWithContext(c, http.MethodPost, endpoint, bytes.NewReader(data))
			if reqErr != nil {
				slog.Warn("otlp_request_invalid", "error", reqErr)
				return
			}
			req.Header.Set("Content-Type", "application/json")
			res, reqErr := http.DefaultClient.Do(req)
			if reqErr != nil {
				slog.Warn("otlp_export_failed", "error", reqErr)
				return
			}
			_ = res.Body.Close()
			if res.StatusCode >= 300 {
				slog.Warn("otlp_export_rejected", "status", res.StatusCode)
			}
		}()
	}
}
