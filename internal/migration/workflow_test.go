package migration

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestValidateRevisionInput(t *testing.T) {
	tests := []struct {
		name  string
		input RevisionInput
		want  error
	}{
		{name: "reversible requires down SQL", input: RevisionInput{Name: "x", UpSQL: "SELECT 1"}, want: ErrInvalidRevision},
		{name: "irreversible requires reason", input: RevisionInput{Name: "x", UpSQL: "SELECT 1", Irreversible: true}, want: ErrIrreversibleReason},
		{name: "valid irreversible", input: RevisionInput{Name: "x", UpSQL: "SELECT 1", Irreversible: true, IrreversibleReason: "external side effect"}},
		{name: "valid reversible", input: validRevision("x")},
		{name: "up SQL contains NUL", input: RevisionInput{Name: "x", UpSQL: "SELECT\x00 1", DownSQL: "SELECT 1"}, want: ErrInvalidRevision},
		{name: "down SQL too large", input: RevisionInput{Name: "x", UpSQL: "SELECT 1", DownSQL: strings.Repeat("x", maxSQLBytes+1)}, want: ErrInvalidRevision},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRevisionInput(tt.input)
			if !errors.Is(err, tt.want) {
				t.Fatalf("validateRevisionInput() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestWorkflowReadDeniesUserWithoutAccessBeforeRepository(t *testing.T) {
	s := NewWorkflowService(nil, fakeAuthz{})
	if _, err := s.ListMigrations(context.Background(), 10, 20); !errors.Is(err, ErrForbidden) {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
}
