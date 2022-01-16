package webhook

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-playground/webhooks/v6/gitlab"
	"go.uber.org/zap"
)

// service declare webhook service.
type service struct {
	logger      *zap.Logger
	hook        *gitlab.Webhook
	publisher   Publisher
	recipientID int64
}

// NewService instantiate new webhook service.
func NewService(logger *zap.Logger, hook *gitlab.Webhook, publisher Publisher, recipientID int64) *service {
	logger.Info("Creation of WebhookService")

	return &service{
		logger:      logger,
		hook:        hook,
		publisher:   publisher,
		recipientID: recipientID,
	}
}

func (s *service) Path() string {
	return "webhook"
}

func (s *service) Methods() []string {
	return []string{"POST"}
}

func (s *service) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, err := s.hook.Parse(r, gitlab.PipelineEvents)
		if err != nil {
			if err != gitlab.ErrEventNotFound {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}

		if err := s.HandlePipeline(r.Context(), payload.(gitlab.PipelineEventPayload)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

// HandlePipeline handles pipeline event.
func (s *service) HandlePipeline(ctx context.Context, pipeline gitlab.PipelineEventPayload) error {
	message := fmt.Sprintf(
		"Project <b>%s</b>\nPipeline %s %s",
		pipeline.Project.Name,
		fmt.Sprintf("%s/pipelines/%d", strings.Trim(pipeline.Project.WebURL, "/"), pipeline.ObjectAttributes.ID),
		pipeline.ObjectAttributes.Status,
	)
	return s.publisher.Publish(ctx, s.recipientID, message)
}
