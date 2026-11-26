// Command benchmark-batch measures a full-table update and a cursor-batched
// update over the same PostgreSQL rows. It only accepts *_test databases.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	rows := flag.Int64("rows", 2_000_000, "number of benchmark rows")
	batchSize := flag.Int("batch-size", 20_000, "rows per batch transaction")
	flag.Parse()
	dsn := os.Getenv("BENCHMARK_DATABASE_URL")
	if dsn == "" {
		log.Fatal("BENCHMARK_DATABASE_URL is required")
	}
	if *rows < 1 || *batchSize < 1 {
		log.Fatal("rows and batch-size must be positive")
	}
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	var databaseName string
	if err = conn.QueryRowContext(ctx, "SELECT current_database()").Scan(&databaseName); err != nil {
		log.Fatal(err)
	}
	if !strings.HasSuffix(databaseName, "_test") {
		log.Fatalf("refusing benchmark database %q: name must end in _test", databaseName)
	}
	if _, err = conn.ExecContext(ctx, `CREATE TEMP TABLE dbmp_batch_benchmark(id BIGINT PRIMARY KEY,processed BOOLEAN NOT NULL DEFAULT FALSE)`); err != nil {
		log.Fatal(err)
	}
	if _, err = conn.ExecContext(ctx, `INSERT INTO dbmp_batch_benchmark(id) SELECT generate_series(1,$1)`, *rows); err != nil {
		log.Fatal(err)
	}
	start := time.Now()
	result, err := conn.ExecContext(ctx, `UPDATE dbmp_batch_benchmark SET processed=TRUE WHERE NOT processed`)
	if err != nil {
		log.Fatal(err)
	}
	nonBatchDuration := time.Since(start)
	nonBatchRows, err := result.RowsAffected()
	if err != nil {
		log.Fatal(err)
	}
	if nonBatchDuration <= time.Second {
		log.Fatalf("unbatched UPDATE took %s for %d rows; increase -rows until it exceeds 1 second", nonBatchDuration, nonBatchRows)
	}
	if _, err = conn.ExecContext(ctx, `UPDATE dbmp_batch_benchmark SET processed=FALSE`); err != nil {
		log.Fatal(err)
	}
	var cursor int64
	var batches, batchedRows int64
	batchStart := time.Now()
	for {
		tx, beginErr := conn.BeginTx(ctx, nil)
		if beginErr != nil {
			log.Fatal(beginErr)
		}
		ids := make([]int64, 0, *batchSize)
		qErr := func() error {
			rows, e := tx.QueryContext(ctx, `SELECT id FROM dbmp_batch_benchmark WHERE id>$1 ORDER BY id LIMIT $2`, cursor, *batchSize)
			if e != nil {
				return e
			}
			defer rows.Close()
			for rows.Next() {
				var id int64
				if e = rows.Scan(&id); e != nil {
					return e
				}
				ids = append(ids, id)
			}
			return rows.Err()
		}()
		if qErr != nil {
			_ = tx.Rollback()
			log.Fatal(qErr)
		}
		if len(ids) == 0 {
			_ = tx.Rollback()
			break
		}
		res, execErr := tx.ExecContext(ctx, `UPDATE dbmp_batch_benchmark SET processed=TRUE WHERE id=ANY($1::bigint[]) AND NOT processed`, ids)
		if execErr != nil {
			_ = tx.Rollback()
			log.Fatal(execErr)
		}
		changed, execErr := res.RowsAffected()
		if execErr != nil {
			_ = tx.Rollback()
			log.Fatal(execErr)
		}
		if changed != int64(len(ids)) {
			_ = tx.Rollback()
			log.Fatalf("batch changed %d of %d selected rows", changed, len(ids))
		}
		if execErr = tx.Commit(); execErr != nil {
			log.Fatal(execErr)
		}
		batches++
		batchedRows += changed
		cursor = ids[len(ids)-1]
	}
	batchDuration := time.Since(batchStart)
	var processed int64
	if err = conn.QueryRowContext(ctx, `SELECT count(*) FROM dbmp_batch_benchmark WHERE processed`).Scan(&processed); err != nil {
		log.Fatal(err)
	}
	if processed != *rows || batchedRows != *rows {
		log.Fatalf("inconsistent results: total=%d batch=%d expected=%d", processed, batchedRows, *rows)
	}
	fmt.Printf("rows=%d\nunbatched_duration=%s\nunbatched_rows=%d\nbatch_size=%d\nbatches=%d\nbatched_duration=%s\nbatched_rows=%d\n", *rows, nonBatchDuration, nonBatchRows, *batchSize, batches, batchDuration, batchedRows)
}
