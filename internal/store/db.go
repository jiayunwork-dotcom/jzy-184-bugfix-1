// Package store implements PostgreSQL persistence and the engine.Tx port.
package store

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"agriheat/internal/engine"
)

//go:embed schema.sql
var schemaSQL string

// DB wraps the connection pool and implements engine.DB.
type DB struct {
	Pool *pgxpool.Pool
}

// Open connects (with retries) and runs migrations.
func Open(ctx context.Context, dsn string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 10
	cfg.MinConns = 1
	cfg.MaxConnLifetime = 6 * time.Hour

	var pool *pgxpool.Pool
	deadline := time.Now().Add(60 * time.Second)
	for {
		pool, err = pgxpool.NewWithConfig(ctx, cfg)
		if err == nil {
			if pingErr := pool.Ping(ctx); pingErr == nil {
				break
			} else {
				err = pingErr
				pool.Close()
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("connect postgres: %w", err)
		}
		time.Sleep(time.Second)
	}
	db := &DB{Pool: pool}
	if err := db.Migrate(ctx); err != nil {
		return nil, err
	}
	return db, nil
}

// Migrate applies the embedded schema (idempotent CREATE TABLE IF NOT EXISTS).
func (db *DB) Migrate(ctx context.Context) error {
	_, err := db.Pool.Exec(ctx, schemaSQL)
	return err
}

// Close releases the pool.
func (db *DB) Close() { db.Pool.Close() }

// BeginTx opens an engine transaction.
func (db *DB) BeginTx() (engine.Tx, error) {
	return db.Begin(context.Background())
}

// Begin opens a pgx transaction wrapped as the engine port.
func (db *DB) Begin(ctx context.Context) (*Tx, error) {
	t, err := db.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &Tx{tx: t, ctx: ctx}, nil
}

// Tx is one PostgreSQL transaction implementing engine.Tx.
type Tx struct {
	tx  pgx.Tx
	ctx context.Context
}

// Commit commits.
func (t *Tx) Commit() error { return t.tx.Commit(t.ctx) }

// Rollback rolls back; safe to call after Commit (error ignored).
func (t *Tx) Rollback() error {
	err := t.tx.Rollback(t.ctx)
	if err == pgx.ErrTxClosed {
		return nil
	}
	return err
}
