package command

import (
	"context"
)

// Publisher declare message publisher.
type Publisher interface {
	// Publish publishes message.
	Publish(ctx context.Context, recipientID int64, msg string) error
}
