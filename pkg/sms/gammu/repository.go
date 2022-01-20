package gammu

import (
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
)

// Here is the example of SMS creation in Gammu via DB:
// https://gist.github.com/tomysmile/7269b6dc508f2dd5742a

const (
	sourceID = "Server Bot 1.0"
)

type repository struct {
	logger *zap.Logger
	db     *sqlx.DB
}

// dbOutbox declare outbox table.
type dbOutbox struct {
	CreatorID         string `db:"CreatorID"`
	MultiPart         bool   `db:"MultiPart"`
	DestinationNumber string `db:"DestinationNumber"`
	UDH               string `db:"UDH"`
	TextDecoded       string `db:"TextDecoded"`
	Coding            string `db:"Coding"`
}

// dbOutboxMultipart declare multipart outbox table.
type dbOutboxMultipart struct {
	ID               int64  `db:"ID"`
	SequencePosition int    `db:"SequencePosition"`
	UDH              string `db:"UDH"`
	TextDecoded      string `db:"TextDecoded"`
	Coding           string `db:"Coding"`
}

// NewRepository creates a new repository object for Gammu DB.
func NewRepository(logger *zap.Logger, db *sqlx.DB) *repository {
	return &repository{
		logger: logger,
		db:     db,
	}
}

// Store stores single-part SMS in DB.
func (r *repository) Store(ctx context.Context, phone string, part MessagePart) error {
	return r.storeOutbox(ctx, r.db, r.to(phone, part))
}

// StoreMultipart stores multi-part SMS in DB.
func (r *repository) StoreMultipart(ctx context.Context, phone string, parts []MessagePart) error {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to start transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // Do rollback in case of any issue

	outbox, p := r.toMultipart(phone, parts)
	if err := r.storeOutbox(ctx, tx, outbox); err != nil {
		return fmt.Errorf("failed to add outbox item: %w", err)
	}
	if err := r.updateID(ctx, tx, p); err != nil {
		return fmt.Errorf("failed to update multipart ID: %w", err)
	}
	for i := range p {
		if err := r.storeOutboxMultipart(ctx, tx, p[i]); err != nil {
			return fmt.Errorf("failed to add outbox multipart item: %w", err)
		}
	}
	return tx.Commit()
}

func (r *repository) to(phone string, part MessagePart) dbOutbox {
	return dbOutbox{
		CreatorID:         sourceID,
		DestinationNumber: phone,
		TextDecoded:       part.Text,
		Coding:            part.Coding,
	}
}

func (r *repository) toMultipart(phone string, parts []MessagePart) (dbOutbox, []dbOutboxMultipart) {
	outbox := dbOutbox{
		CreatorID:         sourceID,
		MultiPart:         true,
		DestinationNumber: phone,
		UDH:               parts[0].UDH,
		TextDecoded:       parts[0].Text,
		Coding:            parts[0].Coding,
	}

	outboxMultipart := make([]dbOutboxMultipart, 0, len(parts)-1)
	for i := 1; i < len(parts); i++ {
		outboxMultipart = append(
			outboxMultipart,
			dbOutboxMultipart{
				SequencePosition: i + 1,
				UDH:              parts[i].UDH,
				TextDecoded:      parts[i].Text,
				Coding:           parts[i].Coding,
			},
		)
	}
	return outbox, outboxMultipart
}

func (r *repository) storeOutbox(ctx context.Context, db sqlx.ExtContext, model dbOutbox) error {
	const query = `
			INSERT INTO outbox ("CreatorID", "MultiPart", "DestinationNumber", "UDH", "TextDecoded", "Coding")
			VALUES (:CreatorID, :MultiPart, :DestinationNumber, :UDH, :TextDecoded, :Coding)`

	if _, err := sqlx.NamedExecContext(ctx, db, query, &model); err != nil {
		return fmt.Errorf("failed to add outbox item: %w", err)
	}
	return nil
}

func (r *repository) updateID(ctx context.Context, db sqlx.QueryerContext, parts []dbOutboxMultipart) error {
	// We are assuming here, that at this point we should not have a lot of parallel transaction.
	// So we can use current value of outbox_id sequence ax ID for multipart.
	const query = `SELECT currval('"outbox_ID_seq"')`
	var id int64
	if err := sqlx.GetContext(ctx, db, &id, query); err != nil {
		return fmt.Errorf("failed to get sequence value: %w", err)
	}
	for i := range parts {
		parts[i].ID = id
	}
	return nil
}

func (r *repository) storeOutboxMultipart(ctx context.Context, db sqlx.ExtContext, model dbOutboxMultipart) error {
	const query = `
			INSERT INTO outbox_multipart ("ID", "SequencePosition", "UDH", "TextDecoded", "Coding")
			VALUES (:ID, :SequencePosition, :UDH, :TextDecoded, :Coding)`

	if _, err := sqlx.NamedExecContext(ctx, db, query, &model); err != nil {
		return fmt.Errorf("failed to add outbox multipart item: %w", err)
	}
	return nil
}
