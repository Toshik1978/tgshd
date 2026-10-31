package command

import (
	"context"

	"github.com/Toshik1978/tgshd/pkg/telegram/route"
)

// Publisher declare message publisher.
type Publisher interface {
	// Publish publishes message.
	Publish(ctx context.Context, dest route.Destination, msg string) error
}
