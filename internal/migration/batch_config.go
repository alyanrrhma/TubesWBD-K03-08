package migration

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/alyanrrhma/TubesWBD-K02-08/internal/worker"
)

// An approved revision selects batch execution by placing this directive at
// the beginning of up_sql: -- dbmp:batch table=TABLE size=N
// SQL must use $1::bigint[] row IDs and affect precisely those IDs.
// The directive is part of the immutable, reviewed SQL revision.
var batchDirective = regexp.MustCompile(`^-- dbmp:batch table=([a-zA-Z_][a-zA-Z_0-9]{0,62}) size=([0-9]+)(?:\r?\n|$)`)

func approvedBatchConfig(upSQL string) (worker.Mode, string, int, error) {
	trimmed := strings.TrimSpace(upSQL)
	if !strings.HasPrefix(trimmed, "-- dbmp:batch") {
		return worker.Normal, "", 0, nil
	}
	matches := batchDirective.FindStringSubmatch(trimmed)
	if matches == nil {
		return "", "", 0, fmt.Errorf("invalid dbmp batch directive")
	}
	size, err := strconv.Atoi(matches[2])
	if err != nil || size < 1 || size > 100000 {
		return "", "", 0, fmt.Errorf("batch size must be 1..100000")
	}
	if !strings.Contains(upSQL, "$1") {
		return "", "", 0, fmt.Errorf("batch SQL must consume $1 ID array")
	}
	return worker.Batch, matches[1], size, nil
}
