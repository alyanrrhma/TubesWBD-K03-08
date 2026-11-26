package platformdb

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestMigrateIsIdempotent(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for i := 0; i < 2; i++ { // kedua kali harus berhasil tanpa efek samping
		if err := Migrate(ctx, db); err != nil {
			t.Fatalf("Migrate #%d: %v", i+1, err)
		}
	}
	var n int
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'database_connections'
		AND column_name IN ('host','port','database_name','username','ssl_mode','password_encrypted')`).Scan(&n)
	if err != nil || n != 6 {
		t.Fatalf("kolom detail koneksi tidak lengkap: n=%d err=%v", n, err)
	}
	if _, err := PurgeExpiredSessions(ctx, db); err != nil {
		t.Fatalf("PurgeExpiredSessions: %v", err)
	}
}

func TestUpgradeM1DataAndResumeAfterMigrationFailure(t *testing.T) {
	dsn := os.Getenv("TEST_PLATFORM_DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("TEST_PLATFORM_DATABASE_URL or DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("m1_upgrade_%d", time.Now().UnixNano())
	if _, err = admin.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if err = db.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	base, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err = execScript(ctx, conn, string(base)); err != nil {
		t.Fatalf("create M1 base schema: %v", err)
	}
	if _, err = conn.ExecContext(ctx, `CREATE TABLE platform_schema_migrations(name TEXT PRIMARY KEY,applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`); err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(schemaFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	var legacyMigrations []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".sql") && entry.Name() <= "0006_fix_rejected_revision_constraint.sql" {
			legacyMigrations = append(legacyMigrations, entry.Name())
		}
	}
	sort.Strings(legacyMigrations)
	for _, name := range legacyMigrations {
		body, e := schemaFS.ReadFile("migrations/" + name)
		if e != nil {
			t.Fatal(e)
		}
		if e = execScript(ctx, conn, string(body)); e != nil {
			t.Fatalf("apply legacy migration %s: %v", name, e)
		}
		if _, e = conn.ExecContext(ctx, `INSERT INTO platform_schema_migrations(name) VALUES($1)`, name); e != nil {
			t.Fatal(e)
		}
	}
	var userID, groupID, databaseID int64
	if err = conn.QueryRowContext(ctx, `INSERT INTO users(username,password_hash,is_platform_admin) VALUES('m1_upgrade_user','legacy-hash',TRUE) RETURNING id`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err = conn.QueryRowContext(ctx, `INSERT INTO app_groups(name,leader_user_id) VALUES('m1_upgrade_group',$1) RETURNING id`, userID).Scan(&groupID); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.ExecContext(ctx, `INSERT INTO user_groups(user_id,group_id) VALUES($1,$2)`, userID, groupID); err != nil {
		t.Fatal(err)
	}
	if err = conn.QueryRowContext(ctx, `INSERT INTO database_connections(name) VALUES('m1_upgrade_connection') RETURNING id`).Scan(&databaseID); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.ExecContext(ctx, `INSERT INTO group_database_access(group_id,database_id,access_level) VALUES($1,$2,'VIEW_ONLY')`, groupID, databaseID); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.ExecContext(ctx, `INSERT INTO migration_history(database_id,version,sql_text,executed_by,status) VALUES($1,7,'ALTER TABLE legacy ADD COLUMN x int',$2,'SUCCESS')`, databaseID, userID); err != nil {
		t.Fatal(err)
	}
	if err = conn.Close(); err != nil {
		t.Fatal(err)
	}

	// Inject a deterministic DDL failure in 0007. The migration runner must not
	// record it as applied; restoring the table must allow a clean retry.
	if _, err = db.ExecContext(ctx, `ALTER TABLE migration_job_attempts RENAME TO migration_job_attempts_saved`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `CREATE VIEW migration_job_attempts AS SELECT * FROM migration_job_attempts_saved`); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, db); err == nil {
		t.Fatal("Migrate succeeded despite injected ALTER TABLE-on-view failure")
	}
	var recorded bool
	if err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM platform_schema_migrations WHERE name='0007_attempt_history_sqlstate.sql')`).Scan(&recorded); err != nil || recorded {
		t.Fatalf("failed migration was recorded: recorded=%v err=%v", recorded, err)
	}
	if _, err = db.ExecContext(ctx, `DROP VIEW migration_job_attempts`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `ALTER TABLE migration_job_attempts_saved RENAME TO migration_job_attempts`); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, db); err != nil {
		t.Fatalf("resume upgrade: %v", err)
	}
	var n int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM platform_schema_migrations WHERE name BETWEEN '0001_connection_material.sql' AND '0011_append_only_execution_records.sql'`).Scan(&n); err != nil || n != 11 {
		t.Fatalf("applied migration count=%d err=%v", n, err)
	}
	var username, passwordHash string
	var isAdmin bool
	if err = db.QueryRowContext(ctx, `SELECT username,password_hash,is_platform_admin FROM users WHERE id=$1`, userID).Scan(&username, &passwordHash, &isAdmin); err != nil || username != "m1_upgrade_user" || passwordHash != "legacy-hash" || !isAdmin {
		t.Fatalf("legacy user changed: %s %s %v err=%v", username, passwordHash, isAdmin, err)
	}
	var preservedGroup, preservedAccess, preservedHistory int
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM user_groups WHERE user_id=$1 AND group_id=$2`, userID, groupID).Scan(&preservedGroup); err != nil || preservedGroup != 1 {
		t.Fatalf("legacy membership=%d err=%v", preservedGroup, err)
	}
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM group_database_access WHERE group_id=$1 AND database_id=$2 AND access_level='VIEW_ONLY'`, groupID, databaseID).Scan(&preservedAccess); err != nil || preservedAccess != 1 {
		t.Fatalf("legacy permission=%d err=%v", preservedAccess, err)
	}
	if err = db.QueryRowContext(ctx, `SELECT count(*) FROM migration_history WHERE database_id=$1 AND version=7 AND sql_text='ALTER TABLE legacy ADD COLUMN x int'`, databaseID).Scan(&preservedHistory); err != nil || preservedHistory != 1 {
		t.Fatalf("legacy migration history=%d err=%v", preservedHistory, err)
	}
}
