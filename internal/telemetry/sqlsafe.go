package telemetry

import (
	"fmt"
	"strings"
	"unicode"
)

// SafeSQL retains statement structure and placeholders but redacts SQL literal
// values, comments (which may contain credentials), and dollar-quoted bodies.
// This is an audit *representation*, not SQL intended to be executed.
// Identifiers are left intact so operators can locate a failing statement.
func SafeSQL(sql string) string {
	var out strings.Builder
	for i := 0; i < len(sql); {
		switch {
		case sql[i] == '\'':
			out.WriteString("'[REDACTED]'")
			i++
			for i < len(sql) {
				// Be conservative with E'...' and non-standard escape modes:
				// a backslash may escape a quote, so skip the following byte.
				if sql[i] == '\\' && i+1 < len(sql) {
					i += 2
					continue
				}
				if sql[i] == '\'' {
					i++
					if i < len(sql) && sql[i] == '\'' {
						i++
						continue
					}
					break
				}
				i++
			}
		case i+1 < len(sql) && sql[i:i+2] == "--":
			out.WriteString("/* comment redacted */")
			i += 2
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
		case i+1 < len(sql) && sql[i:i+2] == "/*":
			out.WriteString("/* comment redacted */")
			i += 2
			depth := 1
			for i < len(sql) && depth > 0 {
				if i+1 < len(sql) && sql[i:i+2] == "/*" {
					depth++
					i += 2
					continue
				}
				if i+1 < len(sql) && sql[i:i+2] == "*/" {
					depth--
					i += 2
					continue
				}
				i++
			}
		case sql[i] == '$':
			// PostgreSQL dollar-quoted literals ($$ or $tag$) are often function
			// bodies. Do not emit their potentially sensitive contents.
			end := i + 1
			for end < len(sql) && (sql[end] == '_' || unicode.IsLetter(rune(sql[end])) || unicode.IsDigit(rune(sql[end]))) {
				end++
			}
			if end < len(sql) && sql[end] == '$' {
				delim := sql[i : end+1]
				if closeAt := strings.Index(sql[end+1:], delim); closeAt >= 0 {
					out.WriteString(delim + "[REDACTED]" + delim)
					i = end + 1 + closeAt + len(delim)
					continue
				}
			}
			out.WriteByte(sql[i])
			i++
		default:
			out.WriteByte(sql[i])
			i++
		}
	}
	return strings.TrimSpace(out.String())
}

// SafeParams preserves argument positions and types without leaking values.
// All values (including IDs, tokens and strings) are redacted deliberately.
func SafeParams(values []any) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = fmt.Sprintf("[REDACTED:%T]", value)
	}
	return out
}
