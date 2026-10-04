package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// testDB returns a pool confined to a fresh schema in the Postgres named by
// PATDB_TEST_DSN, or skips the test when it is unset. Run locally with:
//
//	docker run -d --rm --name patdb-test -e POSTGRES_PASSWORD=test -p 55432:5432 postgres:17.5-alpine
//	PATDB_TEST_DSN=postgres://postgres:test@127.0.0.1:55432/postgres?sslmode=disable go test ./...
func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("PATDB_TEST_DSN")
	if dsn == "" {
		t.Skip("PATDB_TEST_DSN not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("t_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		_, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	return db
}

func appliedMigrations(t *testing.T, db *pgxpool.Pool) []string {
	t.Helper()
	rows, err := db.Query(context.Background(), `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	var versions []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, v)
	}
	return versions
}

func migrationCount(t *testing.T) int {
	t.Helper()
	names, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	return len(names)
}

func TestMigrateFreshDatabaseIsIdempotent(t *testing.T) {
	db := testDB(t)
	a := &app{db: db}
	for i := 0; i < 2; i++ {
		if err := a.migrate(context.Background()); err != nil {
			t.Fatalf("migrate run %d: %v", i, err)
		}
	}
	if got := appliedMigrations(t, db); len(got) != migrationCount(t) || got[0] != "0001_initial" {
		t.Fatalf("applied = %v", got)
	}
	if _, err := db.Exec(context.Background(), `INSERT INTO personal_access_tokens (id, owner_subject, token_hash, token_prefix, name, issued_by) VALUES ('t', 's', '\x01', 'p', 'n', 'agents')`); err != nil {
		t.Fatalf("schema missing columns: %v", err)
	}
}

// A database created by the old inline migrate() has the tables but no
// schema_migrations; 0001 must adopt it without touching existing rows.
func TestMigrateAdoptsLegacySchema(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `CREATE TABLE personal_access_tokens (
 id text PRIMARY KEY, owner_subject text NOT NULL, token_hash bytea NOT NULL UNIQUE,
 token_prefix text NOT NULL, name text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
 expires_at timestamptz, revoked_at timestamptz, last_used_at timestamptz);
 INSERT INTO personal_access_tokens (id, owner_subject, token_hash, token_prefix, name) VALUES ('old', 's', '\x02', 'p', 'n');`); err != nil {
		t.Fatal(err)
	}
	if err := (&app{db: db}).migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var issuedBy string
	if err := db.QueryRow(ctx, `SELECT issued_by FROM personal_access_tokens WHERE id = 'old'`).Scan(&issuedBy); err != nil || issuedBy != "user" {
		t.Fatalf("legacy row issued_by = %q, err = %v", issuedBy, err)
	}
}

func TestMigrateConcurrentReplicas(t *testing.T) {
	db := testDB(t)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- (&app{db: db}).migrate(context.Background())
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := appliedMigrations(t, db); len(got) != migrationCount(t) {
		t.Fatalf("applied = %v", got)
	}
}
