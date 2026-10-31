package route

import (
	"context"
	"strings"
)

// ThreadRejected reports whether err says Telegram refused the message
// because of its forum topic.
func ThreadRejected(err error) bool {
	if err == nil {
		return false
	}
	// The texts with which Telegram rejects a message sent to a missing,
	// closed or deleted forum topic, lower-cased.
	text := strings.ToLower(err.Error())
	for _, s := range []string{"message thread not found", "topic_closed", "topic_deleted"} {
		if strings.Contains(text, s) {
			return true
		}
	}

	return false
}

// SendWithFallback calls send for d. When d has a thread and send fails with a
// thread rejection, warn is called and send is retried once without the
// thread, the sound unchanged. Any other error is returned as is.
func SendWithFallback(
	ctx context.Context,
	d Destination,
	send func(context.Context, Destination) error,
	warn func(threadID int, err error),
) error {
	err := send(ctx, d)
	if err == nil || d.ThreadID == 0 || !ThreadRejected(err) {
		return err
	}
	warn(d.ThreadID, err)
	d.ThreadID = 0

	return send(ctx, d)
}
