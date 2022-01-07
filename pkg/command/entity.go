package command

import (
	"context"
)

// Handler define handler for specific command.
type Handler interface {
	// Name returns name of command to handle.
	Name() string
	// Handle handles command.
	Handle(ctx context.Context, telegramID int64, args string) error
}
