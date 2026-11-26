package migration

import (
	"github.com/alyanrrhma/TubesWBD-K02-08/internal/worker"
	"testing"
)

func TestApprovedBatchConfig(t *testing.T) {
	cases := []struct {
		sql   string
		mode  worker.Mode
		table string
		size  int
		valid bool
	}{
		{"UPDATE records SET active=true", worker.Normal, "", 0, true},
		{"-- dbmp:batch table=records size=250\nUPDATE records SET active=true WHERE id=ANY($1::bigint[])", worker.Batch, "records", 250, true},
		{"-- dbmp:batch table=records size=0\nSELECT $1", "", "", 0, false},
		{"-- dbmp:batch table=records;DROP size=1\nSELECT $1", "", "", 0, false},
		{"-- dbmp:batch table=records size=100001\nSELECT $1", "", "", 0, false},
		{"-- dbmp:batch table=records size=5\nUPDATE records SET x=1", "", "", 0, false},
	}
	for _, tc := range cases {
		mode, table, size, err := approvedBatchConfig(tc.sql)
		if (err == nil) != tc.valid || (tc.valid && (mode != tc.mode || table != tc.table || size != tc.size)) {
			t.Errorf("SQL %q: got %s/%s/%d err=%v", tc.sql, mode, table, size, err)
		}
	}
}
