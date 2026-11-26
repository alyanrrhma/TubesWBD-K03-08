package migration

import (
	"errors"
	"testing"
)

func TestValidateRevisionInput(t *testing.T) {
	tests := []struct {
		name  string
		input RevisionInput
		want  error
	}{
		{name: "reversible requires down SQL", input: RevisionInput{Name: "x", UpSQL: "SELECT 1"}, want: ErrWorkflowInvalid},
		{name: "irreversible requires reason", input: RevisionInput{Name: "x", UpSQL: "SELECT 1", Irreversible: true}, want: ErrIrreversibleReason},
		{name: "valid irreversible", input: RevisionInput{Name: "x", UpSQL: "SELECT 1", Irreversible: true, IrreversibleReason: "external side effect"}},
		{name: "valid reversible", input: validRevision("x")},
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
