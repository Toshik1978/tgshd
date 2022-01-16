package sms

import (
	"context"
	"encoding/json"
	"net/http"

	"go.uber.org/zap"
)

type Publisher interface {
	Publish(ctx context.Context, phone, msg string) error
}

// service declare sms service.
type service struct {
	logger    *zap.Logger
	publisher Publisher
}

// NewService instantiate new sms service.
func NewService(logger *zap.Logger, publisher Publisher) *service {
	logger.Info("Creation of SmsService")

	return &service{
		logger:    logger,
		publisher: publisher,
	}
}

func (s *service) Path() string {
	return "sms"
}

func (s *service) Methods() []string {
	return []string{"POST"}
}

func (s *service) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil {
			http.Error(w, "no body in request", http.StatusBadRequest)
			return
		}
		defer func() { _ = r.Body.Close() }()

		var msg Message
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.publisher.Publish(r.Context(), msg.Phone, msg.Text); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
	})
}
