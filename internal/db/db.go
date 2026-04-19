// Package db is the Postgres data layer for the engine.
//
// Queries are hand-written against pgx/v5 to keep the iteration loop
// tight while the schema churns. Once the shape stabilizes we may move
// to sqlc-generated code, but the Store contract will stay the same.
package db

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// ErrNotFound signals a missing row.
var ErrNotFound = errors.New("db: not found")

// Pool wraps *pgxpool.Pool with engine-specific helpers.
type Pool struct {
	*pgxpool.Pool
}

// Open dials Postgres and returns a configured pool.
func Open(ctx context.Context, url string, maxConns int32) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse db url: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	return &Pool{Pool: pool}, nil
}

// Migrate applies all embedded up-migrations in order. Each file is run
// inside its own transaction. Already-applied migrations are tracked in
// the schema_migrations table.
func (p *Pool) Migrate(ctx context.Context) error {
	if _, err := p.Exec(ctx, `
        CREATE TABLE IF NOT EXISTS schema_migrations (
            version TEXT PRIMARY KEY,
            applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
        )`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	files, err := listMigrations("migrations")
	if err != nil {
		return err
	}
	for _, f := range files {
		var exists bool
		if err := p.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`,
			f.version).Scan(&exists); err != nil {
			return fmt.Errorf("check migration %s: %w", f.version, err)
		}
		if exists {
			continue
		}
		if err := p.runMigration(ctx, f); err != nil {
			return fmt.Errorf("apply %s: %w", f.version, err)
		}
	}
	return nil
}

func (p *Pool) runMigration(ctx context.Context, f migrationFile) error {
	tx, err := p.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, f.sql); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version) VALUES ($1)`, f.version); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type migrationFile struct {
	version string
	sql     string
}

func listMigrations(dir string) ([]migrationFile, error) {
	entries, err := fs.ReadDir(migrationsFS, dir)
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	var ups []migrationFile
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		raw, err := migrationsFS.ReadFile(dir + "/" + name)
		if err != nil {
			return nil, err
		}
		version := strings.TrimSuffix(name, ".up.sql")
		ups = append(ups, migrationFile{version: version, sql: string(raw)})
	}
	sort.Slice(ups, func(i, j int) bool { return ups[i].version < ups[j].version })
	return ups, nil
}

// mapErr normalizes pgx errors to package-level sentinels.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
