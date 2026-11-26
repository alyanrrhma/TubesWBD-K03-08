package telemetry

import (
	"context"
	"strings"
	"testing"
)

func TestTraceContextPropagation(t *testing.T) {
	in := "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"
	ctx := FromHeader(context.Background(), in)
	if Traceparent(ctx) != in {
		t.Fatal("valid traceparent not preserved")
	}
	child, end := Start(ctx, "test", nil)
	defer end(nil)
	if TraceID(child) != "0123456789abcdef0123456789abcdef" {
		t.Fatal("missing parent trace ID")
	}
	if Traceparent(child) == in || !strings.HasPrefix(Traceparent(child), "00-0123456789abcdef0123456789abcdef-") {
		t.Fatal("child span not generated")
	}
	invalid := FromHeader(context.Background(), "00-00000000000000000000000000000000-0123456789abcdef-01")
	if Traceparent(invalid) != "" {
		t.Fatal("zero trace ID accepted")
	}
}
