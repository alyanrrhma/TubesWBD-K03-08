package telemetry

import (
	"strings"
	"testing"
)

func TestSafeSQLRemovesLiteralsAndComments(t *testing.T) {
	for _, statement := range []string{
		"SELECT * FROM users WHERE api_key='very-secret' -- private-key=123",
		"SELECT $$another-secret$$, $tag$hidden-data$tag$",
		"/* secret */ SELECT 'hidden' /* also-secret */",
		"SELECT 'password: ''secret''', $1::bigint",
		"SELECT E'escaped \\'secret-more'",
	} {
		safe := SafeSQL(statement)
		for _, forbidden := range []string{"very-secret", "private-key", "another-secret", "hidden-data", "also-secret", "'hidden'", "secret''", "secret-more"} {
			if strings.Contains(safe, forbidden) {
				t.Fatalf("literal leaked from %q: %q", statement, safe)
			}
		}
	}
	if got := SafeSQL("SELECT * FROM public.items WHERE id = $1"); got != "SELECT * FROM public.items WHERE id = $1" {
		t.Fatalf("query shape changed unexpectedly: %s", got)
	}
}

func TestSafeParamsNeverIncludeValues(t *testing.T) {
	params := SafeParams([]any{"my-password-123", int64(123456), []int64{7, 8}})
	joined := strings.Join(params, ",")
	for _, leak := range []string{"my-password-123", "123456", "7, 8"} {
		if strings.Contains(joined, leak) {
			t.Fatalf("parameter leaked: %s", joined)
		}
	}
	if len(params) != 3 {
		t.Fatal("parameter count changed")
	}
}
