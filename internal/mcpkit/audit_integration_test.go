//go:build integration

package mcpkit_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/abhishekjha/close-copilot/internal/store"
)

func setupAuditDB(t *testing.T) (*store.Store, string) {
	t.Helper()
	ctx := context.Background()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		pg, err := postgres.Run(ctx, "pgvector/pgvector:pg17",
			postgres.WithDatabase("test_copilot"),
			postgres.WithUsername("copilot"),
			postgres.WithPassword("copilot"),
			postgres.BasicWaitStrategies(),
		)
		if err != nil {
			t.Fatalf("start postgres: %v", err)
		}
		t.Cleanup(func() { _ = testcontainers.TerminateContainer(pg) })
		if url, err = pg.ConnectionString(ctx, "sslmode=disable"); err != nil {
			t.Fatalf("connection string: %v", err)
		}
	}
	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(st.Close)
	dir := filepath.Join("..", "..", "migrations")
	for i := 0; i < 2; i++ { // the second run must be a no-op
		if err := st.Migrate(ctx, dir); err != nil {
			t.Fatalf("migrate (run %d): %v", i+1, err)
		}
	}
	return st, url
}

func TestAudit_RealPostgres(t *testing.T) {
	ctx := context.Background()
	st, url := setupAuditDB(t)
	sess := startAudited(t, st)

	call(t, sess, "struct_tool", map[string]any{"company": "sharma"})
	call(t, sess, "slice_tool", map[string]any{"company": "sharma", "actor": "admin"})

	rows, err := st.ListAuditLogs(ctx, 10)
	if err != nil {
		t.Fatalf("ListAuditLogs: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows: got %d, want 2", len(rows))
	}
	for _, r := range rows {
		if r.Actor != "agent" || r.Server == nil || *r.Server != "books" || r.Tool == nil {
			t.Errorf("row: %+v", r)
		}
	}
	_ = url
}

func TestAudit_AppRoleAppendOnly(t *testing.T) {
	ctx := context.Background()
	_, url := setupAuditDB(t)
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	// Each statement runs in its own tx so a failure does not poison the next.
	run := func(stmt string) error {
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE copilot_app"); err != nil {
			t.Fatalf("set role: %v", err)
		}
		_, err = tx.Exec(ctx, stmt)
		return err
	}

	if err := run(`INSERT INTO audit_log (actor, server, tool) VALUES ('t','s','x')`); err != nil {
		t.Errorf("INSERT: %v", err)
	}
	if err := run(`SELECT count(*) FROM audit_log`); err != nil {
		t.Errorf("SELECT: %v", err)
	}
	for _, stmt := range []string{
		`UPDATE audit_log SET actor = 'x'`,
		`DELETE FROM audit_log`,
		`TRUNCATE audit_log`,
	} {
		err := run(stmt)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Errorf("%s: want SQLSTATE 42501, got %v", stmt, err)
		}
	}
}
