package command

import (
	"context"
)

// Handler define handler for specific command.
type Handler interface {
	// Name returns name of command to handle.
	Name() string
	// Handle handles command.
	Handle(ctx context.Context, senderID int64) error
}

// Publisher declare message publisher.
type Publisher interface {
	// Publish publishes message.
	Publish(ctx context.Context, recipientID int64, msg string) error
}
