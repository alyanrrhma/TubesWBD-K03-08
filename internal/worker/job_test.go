package worker

import (
	"errors"
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	j := Job{DatabaseID: 1, MigrationID: 1, Revision: 1, ActorID: 1, Operation: Apply, Mode: Batch, SQL: `UPDATE items SET flag=true WHERE id=ANY($1::bigint[])`, BatchTable: "items", BatchSize: 250}
	if err := j.Validate(); err != nil {
		t.Fatal(err)
	}
	j.BatchTable = "items;DROP TABLE users"
	if !errors.Is(j.Validate(), ErrInvalidJob) {
		t.Fatal("unsafe SQL identifier accepted")
	}
	j.BatchTable = "items"
	j.Operation = Rollback
	if !errors.Is(j.Validate(), ErrInvalidJob) {
		t.Fatal("rollback without immutable SQL down/source accepted")
	}
	j.SourceExecutionID = "ex"
	j.ReverseSQL = `UPDATE items SET flag=false WHERE id=ANY($1::bigint[])`
	if err := j.Validate(); err != nil {
		t.Fatal(err)
	}
}
func TestBackoffBounded(t *testing.T) {
	if Backoff(1) != time.Second || Backoff(2) != 2*time.Second || Backoff(100) != 64*time.Second {
		t.Fatal("incorrect retry backoff")
	}
}
func TestFailClosed(t *testing.T) {
	w := Worker{}
	if w.check() == nil {
		t.Fatal("worker must not run without domain validator")
	}
}
