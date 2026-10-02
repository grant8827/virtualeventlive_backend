package database

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations
var migrationsFS embed.FS

// migrationLockID is the advisory lock that serializes migrations, so two
// instances (or test processes) starting together don't run the same DDL
// concurrently and deadlock.
const migrationLockID = 727_301_002

func RunMigrations(db *pgxpool.Pool) error {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("reading migrations dir: %w", err)
	}
	ctx := context.Background()
	conn, err := db.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquiring migration connection: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return fmt.Errorf("taking migration lock: %w", err)
	}
	defer conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, migrationLockID) //nolint:errcheck

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		content, err := fs.ReadFile(migrationsFS, "migrations/"+entry.Name())
		if err != nil {
			return fmt.Errorf("reading %s: %w", entry.Name(), err)
		}
		if _, err := conn.Exec(ctx, string(content)); err != nil {
			return fmt.Errorf("migration %s: %w", entry.Name(), err)
		}
	}
	return nil
}
