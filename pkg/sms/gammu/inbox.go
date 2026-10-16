package gammu

import (
	"context"
	"fmt"
	"time"
)

// Unprocessed rows only, oldest first. UDH may be NULL in the gammu schema.
const selectInbox = `
	SELECT "ID", "SenderNumber", "TextDecoded", COALESCE("UDH", ''), "ReceivingDateTime"
	FROM inbox
	WHERE "Processed" = false
	ORDER BY "ReceivingDateTime", "ID"`

const markInboxProcessed = `UPDATE inbox SET "Processed" = true WHERE "ID" = ANY($1)`

// InboxMessage is one received SMS part stored in the gammu inbox.
type InboxMessage struct {
	ID         int64
	Sender     string
	Text       string
	UDH        string
	ReceivedAt time.Time
}

// rowScanner is satisfied by *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// Unprocessed returns the received SMS parts that have not been marked processed yet.
func (r *repository) Unprocessed(ctx context.Context) ([]InboxMessage, error) {
	rows, err := r.db.QueryContext(ctx, selectInbox)
	if err != nil {
		return nil, fmt.Errorf("failed to query inbox: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var messages []InboxMessage
	for rows.Next() {
		msg, err := scanInboxMessage(rows)
		if err != nil {
			return nil, err
		}
		messages = append(messages, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read inbox: %w", err)
	}

	return messages, nil
}

// MarkProcessed marks all the given inbox rows as processed in a single statement.
func (r *repository) MarkProcessed(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := r.db.ExecContext(ctx, markInboxProcessed, ids); err != nil {
		return fmt.Errorf("failed to mark inbox items processed: %w", err)
	}

	return nil
}

func scanInboxMessage(row rowScanner) (InboxMessage, error) {
	var msg InboxMessage
	if err := row.Scan(&msg.ID, &msg.Sender, &msg.Text, &msg.UDH, &msg.ReceivedAt); err != nil {
		return InboxMessage{}, fmt.Errorf("failed to scan inbox item: %w", err)
	}
	// gammu's schema stores "ReceivingDateTime" as a timestamp without time zone holding the
	// server's local time, which the driver hands over as that wall clock in UTC.
	msg.ReceivedAt = wallClockIn(msg.ReceivedAt, time.Local) //nolint:gosmopolitan // gammu stores local time

	return msg, nil
}

// wallClockIn returns the instant whose wall clock in loc reads the same as t's.
func wallClockIn(t time.Time, loc *time.Location) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), loc)
}
