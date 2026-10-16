package telegram

import (
	"context"
	"fmt"
	"time"
)

// retryUntil calls attempt until it succeeds, waiting between failures with a
// delay that starts at initial and doubles up to limit. onFail is told about
// each failure and the delay before the next attempt. It returns the context
// error when ctx ends first.
func retryUntil(
	ctx context.Context, initial, limit time.Duration, attempt func() error, onFail func(error, time.Duration),
) error {
	delay := initial
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("retry stopped: %w", err)
		}
		err := attempt()
		if err == nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("retry stopped: %w", ctxErr)
		}
		onFail(err, delay)

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()

			return fmt.Errorf("retry stopped: %w", ctx.Err())
		case <-timer.C:
		}
		delay = min(delay*2, limit)
	}
}
