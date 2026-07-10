package command

import (
	"context"
	"fmt"
	"html"
	"strings"

	"go.uber.org/zap"
)

// SmsSender declares the outgoing SMS backend (gammu-smsd).
type SmsSender interface {
	Publish(ctx context.Context, phone, msg string) error
}

type smsCommand struct {
	logger    *zap.Logger
	publisher Publisher
	sender    SmsSender
	chatID    int64
}

// NewSmsCommand creates a new handler for the /sms command.
func NewSmsCommand(logger *zap.Logger, publisher Publisher, sender SmsSender, chatID int64) *smsCommand {
	logger.Info("SMS command created")
	return &smsCommand{
		logger:    logger,
		publisher: publisher,
		sender:    sender,
		chatID:    chatID,
	}
}

func (c *smsCommand) Name() string {
	return "sms"
}

func (c *smsCommand) Enabled() bool {
	return true
}

func (c *smsCommand) Handle(ctx context.Context, senderID int64, cmd string) error {
	if senderID != c.chatID {
		c.logger.With(zap.Int64("sender_id", senderID)).Debug("Unauthorized /sms attempt")
		return nil
	}

	phone, msg := parseSmsArgs(cmd)
	if phone == "" || msg == "" {
		if err := c.publisher.Publish(ctx, senderID, "Usage: /sms &lt;phone&gt; &lt;message&gt;"); err != nil {
			return fmt.Errorf("failed to publish reply: %w", err)
		}
		return nil
	}

	if err := c.sender.Publish(ctx, phone, msg); err != nil {
		if replyErr := c.publisher.Publish(ctx, senderID,
			fmt.Sprintf("Failed to send SMS: %s", html.EscapeString(err.Error()))); replyErr != nil {
			c.logger.With(zap.Error(replyErr)).Debug("Failed to send /sms failure reply")
		}
		return fmt.Errorf("failed to send sms: %w", err)
	}

	if err := c.publisher.Publish(ctx, senderID, fmt.Sprintf("SMS queued for <b>%s</b>", html.EscapeString(phone))); err != nil {
		return fmt.Errorf("failed to publish reply: %w", err)
	}
	return nil
}

// parseSmsArgs extracts the phone number and message body from the raw command
// text, e.g. "/sms +1234567890 Hello world" -> ("+1234567890", "Hello world").
func parseSmsArgs(cmd string) (string, string) {
	fields := strings.Fields(cmd)
	if len(fields) < 3 {
		return "", ""
	}
	return fields[1], strings.Join(fields[2:], " ")
}
