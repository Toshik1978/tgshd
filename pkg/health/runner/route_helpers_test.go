package runner

import (
	"context"
	"sync"

	"github.com/Toshik1978/tgshd/pkg/health/notify"
	"github.com/Toshik1978/tgshd/pkg/telegram/route"
)

// telegramAdapter presents a text-only fake as a TelegramSender and records
// the route of every send.
type telegramAdapter struct {
	next notify.Sender

	mu     sync.Mutex
	routes []route.Route
}

// asTelegram wraps next as a TelegramSender.
func asTelegram(next notify.Sender) *telegramAdapter { return &telegramAdapter{next: next} }

func (a *telegramAdapter) Send(ctx context.Context, r route.Route, text string) error {
	a.mu.Lock()
	a.routes = append(a.routes, r)
	a.mu.Unlock()

	return a.next.Send(ctx, text)
}

// sentRoutes returns the routes of the sends so far, in order.
func (a *telegramAdapter) sentRoutes() []route.Route {
	a.mu.Lock()
	defer a.mu.Unlock()

	return append([]route.Route(nil), a.routes...)
}
