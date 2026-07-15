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

func RunMigrations(db *pgxpool.Pool) error {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("reading migrations dir: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		content, err := fs.ReadFile(migrationsFS, "migrations/"+entry.Name())
		if err != nil {
			return fmt.Errorf("reading %s: %w", entry.Name(), err)
		}
		if _, err := db.Exec(context.Background(), string(content)); err != nil {
			return fmt.Errorf("migration %s: %w", entry.Name(), err)
		}
	}
	return nil
}
