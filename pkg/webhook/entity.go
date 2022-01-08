package webhook

import (
	"context"
	"net/http"
)

// Service declare webhook service.
type Service interface {
	// Handler return http handler.
	Handler() http.Handler
}

// Publisher declare message publisher.
type Publisher interface {
	// Publish publishes message.
	Publish(ctx context.Context, recipientID int64, msg string) error
}
