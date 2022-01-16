package sms

import (
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"
)

func (g *gammu) buildOutbox(ctx context.Context, phone string, part gammuPart) error {
	return g.storeOutbox(ctx, g.db, toModel(phone, part))
}

func (g *gammu) storeOutbox(ctx context.Context, db sqlx.ExtContext, model GammuOutbox) error {
	const query = `
			INSERT INTO outbox ("CreatorID", "MultiPart", "DestinationNumber", "UDH", "TextDecoded", "Coding")
			VALUES (:CreatorID, :MultiPart, :DestinationNumber, :UDH, :TextDecoded, :Coding)`

	if _, err := sqlx.NamedExecContext(ctx, db, query, &model); err != nil {
		return fmt.Errorf("failed to add outbox item: %w", err)
	}
	return nil
}

func (g *gammu) buildMultipartOutbox(ctx context.Context, phone string, parts []gammuPart) error {
	tx, err := g.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to start transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // Do rollback in case of any issue

	outbox, multiparts := toModels(phone, parts)
	if err := g.storeOutbox(ctx, tx, outbox); err != nil {
		return fmt.Errorf("failed to add outbox item: %w", err)
	}
	if err := g.updateID(ctx, tx, multiparts); err != nil {
		return fmt.Errorf("failed to update multipart ID: %w", err)
	}
	for i := range multiparts {
		if err := g.storeOutboxMultipart(ctx, tx, multiparts[i]); err != nil {
			return fmt.Errorf("failed to add outbox multipart item: %w", err)
		}
	}
	return tx.Commit()
}

func (g *gammu) updateID(ctx context.Context, db sqlx.QueryerContext, multiparts []GammuOutboxMultipart) error {
	// We are assuming here, that at this point we should not have a lot of parallel transaction.
	// So we can use current value of outbox_id sequence ax ID for multipart.
	const query = `SELECT currval('"outbox_ID_seq"')`
	var id int64
	if err := sqlx.GetContext(ctx, db, &id, query); err != nil {
		return fmt.Errorf("failed to get sequence value: %w", err)
	}
	for i := range multiparts {
		multiparts[i].ID = id
	}
	return nil
}

func (g *gammu) storeOutboxMultipart(ctx context.Context, db sqlx.ExtContext, model GammuOutboxMultipart) error {
	const query = `
			INSERT INTO outbox_multipart ("ID", "SequencePosition", "UDH", "TextDecoded", "Coding")
			VALUES (:ID, :SequencePosition, :UDH, :TextDecoded, :Coding)`

	if _, err := sqlx.NamedExecContext(ctx, db, query, &model); err != nil {
		return fmt.Errorf("failed to add outbox multipart item: %w", err)
	}
	return nil
}
