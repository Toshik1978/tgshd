package gammu

import (
	"context"
	"database/sql"
	"fmt"

	"go.uber.org/zap"
)

// Gammu DB outbox reference:
// https://gist.github.com/tomysmile/7269b6dc508f2dd5742a
const creatorID = "tgshd"

const insertOutbox = `
	INSERT INTO outbox ("CreatorID", "MultiPart", "DestinationNumber", "UDH", "TextDecoded", "Coding")
	VALUES ($1, $2, $3, $4, $5, $6)`

const insertOutboxMultipart = `
	INSERT INTO outbox_multipart ("ID", "SequencePosition", "UDH", "TextDecoded", "Coding")
	VALUES ($1, $2, $3, $4, $5)`

const selectCurrval = `SELECT currval('"outbox_ID_seq"')`

// execer is satisfied by both *sql.DB and *sql.Tx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

type repository struct {
	logger *zap.Logger
	db     *sql.DB
}

// NewRepository creates a new repository over the gammu database.
func NewRepository(logger *zap.Logger, db *sql.DB) *repository {
	return &repository{logger: logger, db: db}
}

// Store stores a single-part SMS in the outbox.
func (r *repository) Store(ctx context.Context, phone string, part MessagePart) error {
	return storeOutbox(ctx, r.db, false, phone, part)
}

// StoreMultipart stores a multi-part SMS across outbox + outbox_multipart in one transaction.
func (r *repository) StoreMultipart(ctx context.Context, phone string, parts []MessagePart) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to start transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit

	if err := storeOutbox(ctx, tx, true, phone, parts[0]); err != nil {
		return fmt.Errorf("failed to add outbox item: %w", err)
	}

	var id int64
	if err := tx.QueryRowContext(ctx, selectCurrval).Scan(&id); err != nil {
		return fmt.Errorf("failed to get sequence value: %w", err)
	}

	for i := 1; i < len(parts); i++ {
		if _, err := tx.ExecContext(ctx, insertOutboxMultipart,
			id, i+1, parts[i].UDH, parts[i].Text, parts[i].Coding); err != nil {
			return fmt.Errorf("failed to add outbox multipart item: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}
	return nil
}

func storeOutbox(ctx context.Context, db execer, multipart bool, phone string, part MessagePart) error {
	if _, err := db.ExecContext(ctx, insertOutbox,
		creatorID, multipart, phone, part.UDH, part.Text, part.Coding); err != nil {
		return fmt.Errorf("failed to add outbox item: %w", err)
	}
	return nil
}
