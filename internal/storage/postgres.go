package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgErrUniqueViolation is the PostgreSQL SQLSTATE code for unique_violation.
const pgErrUniqueViolation = "23505"

// SQL statements used by the Postgres implementation.
// Keeping them as package-level constants makes them visible during code
// review and avoids repeated string allocation at call sites.
const (
	sqlSaveMapping = `
INSERT INTO message_mappings
    (pair_key, source_chat_id, source_message_id,
     target_chat_id, target_message_id,
     direction, media_group_id, account_phone)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id`

	sqlFindBySource = `
SELECT id, pair_key,
       source_chat_id, source_message_id,
       target_chat_id, target_message_id,
       direction, media_group_id, account_phone, created_at
FROM message_mappings
WHERE source_chat_id = $1
  AND source_message_id = $2
LIMIT 1`

	sqlFindByTarget = `
SELECT id, pair_key,
       source_chat_id, source_message_id,
       target_chat_id, target_message_id,
       direction, media_group_id, account_phone, created_at
FROM message_mappings
WHERE target_chat_id = $1
  AND target_message_id = $2
LIMIT 1`

	sqlFindByMediaGroup = `
SELECT id, pair_key,
       source_chat_id, source_message_id,
       target_chat_id, target_message_id,
       direction, media_group_id, account_phone, created_at
FROM message_mappings
WHERE media_group_id = $1`
)

// Postgres implements Storage using a pgxpool connection pool.
type Postgres struct {
	pool *pgxpool.Pool
}

// New creates a new Postgres storage backed by a pgxpool.Pool.
//
// Parameters:
//   - ctx      — context used for the initial connection and ping.
//   - dsn      — PostgreSQL connection string (e.g. postgres://user:pass@host/db).
//   - maxConns — maximum number of connections in the pool (pool_max_conns).
//   - minConns — minimum number of idle connections kept alive (pool_min_conns).
//
// Returns an error if the configuration cannot be parsed, the pool cannot be
// created, or the database does not respond to a ping.
func New(ctx context.Context, dsn string, maxConns, minConns int32) (Storage, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: parse dsn: %w", err)
	}

	cfg.MaxConns = maxConns
	cfg.MinConns = minConns

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("storage: create pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("storage: ping: %w", err)
	}

	return &Postgres{pool: pool}, nil
}

// SaveMapping inserts m into the database and sets m.ID from the RETURNING clause.
// Returns ErrDuplicate when the unique constraint on (source_chat_id, source_message_id)
// is violated.
func (p *Postgres) SaveMapping(ctx context.Context, m MessageMapping) error {
	var id int64
	err := p.pool.QueryRow(ctx, sqlSaveMapping,
		m.PairKey,
		m.SourceChatID,
		m.SourceMessageID,
		m.TargetChatID,
		m.TargetMessageID,
		string(m.Direction),
		m.MediaGroupID,
		m.AccountPhone,
	).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgErrUniqueViolation {
			return ErrDuplicate
		}
		return fmt.Errorf("storage: save mapping: %w", err)
	}
	return nil
}

// FindBySource returns the mapping whose source coordinates match (chatID, messageID).
// Returns (nil, nil) when no row is found.
func (p *Postgres) FindBySource(ctx context.Context, chatID, messageID int64) (*MessageMapping, error) {
	rows, err := p.pool.Query(ctx, sqlFindBySource, chatID, messageID)
	if err != nil {
		return nil, fmt.Errorf("storage: find by source: %w", err)
	}

	m, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[MessageMapping])
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("storage: find by source: %w", err)
	}
	return &m, nil
}

// FindByTarget returns the mapping whose target coordinates match (chatID, messageID).
// Returns (nil, nil) when no row is found.
func (p *Postgres) FindByTarget(ctx context.Context, chatID, messageID int64) (*MessageMapping, error) {
	rows, err := p.pool.Query(ctx, sqlFindByTarget, chatID, messageID)
	if err != nil {
		return nil, fmt.Errorf("storage: find by target: %w", err)
	}

	m, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[MessageMapping])
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("storage: find by target: %w", err)
	}
	return &m, nil
}

// FindByMediaGroup returns all mappings that share the given mediaGroupID.
// Returns an empty (non-nil) slice when no rows are found.
func (p *Postgres) FindByMediaGroup(ctx context.Context, mediaGroupID string) ([]MessageMapping, error) {
	rows, err := p.pool.Query(ctx, sqlFindByMediaGroup, mediaGroupID)
	if err != nil {
		return nil, fmt.Errorf("storage: find by media group: %w", err)
	}

	mappings, err := pgx.CollectRows(rows, pgx.RowToStructByName[MessageMapping])
	if err != nil {
		return nil, fmt.Errorf("storage: find by media group: %w", err)
	}
	if mappings == nil {
		mappings = []MessageMapping{}
	}
	return mappings, nil
}

// Ping checks that the database is reachable by acquiring a connection from
// the pool. Used by the application health-check goroutine.
func (p *Postgres) Ping(ctx context.Context) error {
	return p.pool.Ping(ctx)
}

// Close drains and closes the underlying connection pool.
func (p *Postgres) Close() error {
	p.pool.Close()
	return nil
}
