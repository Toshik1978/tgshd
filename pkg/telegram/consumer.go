package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"gopkg.in/telebot.v4"

	"github.com/Toshik1978/tgshd/pkg/telegram/route"
)

// Handler define handler for specific command.
type Handler interface {
	// Name returns name of command to handle.
	Name() string
	// Enabled return true if command is enabled.
	Enabled() bool
	// Handle handles command.
	Handle(ctx context.Context, origin route.Origin, cmd string) error
}

type consumer struct {
	logger   *zap.Logger
	bot      *telebot.Bot
	access   *route.AccessList
	handlers []Handler

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	polling bool
}

const (
	connectInitialDelay = 2 * time.Second
	connectMaxDelay     = time.Minute
)

// NewConsumer initializes new consumer for telegram messages.
func NewConsumer(logger *zap.Logger, bot *telebot.Bot, access *route.AccessList, handlers []Handler) *consumer {
	logger.Info("Creation of TelegramConsumer")

	if access.Empty() {
		logger.Warn("Telegram allow-list is empty, set TELEGRAM_CHAT_ID or TELEGRAM_ALLOWED_IDS")
	}

	return &consumer{
		logger:   logger,
		bot:      bot,
		access:   access,
		handlers: handlers,
	}
}

func (c *consumer) Start(ctx context.Context) error {
	c.logger.Info("Telegram bot started")

	// Add commands.
	enabled := make(map[string]Handler)
	for _, handler := range c.handlers {
		if handler.Enabled() {
			names := strings.SplitSeq(handler.Name(), "|")
			for name := range names {
				c.logger.With(zap.String("command", name)).Info("Telegram command added")
				c.bot.Handle("/"+name, func(tctx telebot.Context) error {
					return c.handleCommand(ctx, handler, tctx)
				})
				enabled["/"+name] = handler
			}
		}
	}
	// Add generic handler in case we are in the channel.
	c.bot.Handle(telebot.OnChannelPost, func(tctx telebot.Context) error {
		return c.handleCommands(ctx, enabled, tctx)
	})

	// The bot is created offline, so Telegram being unreachable at boot must
	// not stop the daemon: connect and start polling in the background.
	runCtx, cancel := context.WithCancel(context.Background())
	c.mu.Lock()
	c.cancel = cancel
	c.done = make(chan struct{})
	done := c.done
	c.mu.Unlock()

	go func() {
		defer close(done)
		c.connectAndPoll(runCtx)
	}()

	return nil
}

func (c *consumer) Stop(ctx context.Context) error {
	// Cancelling under the lock means the connect goroutine either already
	// marked itself polling or will see the cancellation and never start.
	c.mu.Lock()
	done, polling := c.done, c.polling
	if c.cancel != nil {
		c.cancel()
	}
	c.mu.Unlock()

	shutdownChannel := make(chan any)
	go func() {
		if polling {
			c.bot.Stop()
		}
		if done != nil {
			<-done
		}
		close(shutdownChannel)
		c.logger.Info("Telegram bot gracefully stopped")
	}()

	select {
	case <-shutdownChannel:
	case <-ctx.Done():
		return context.DeadlineExceeded
	}

	return nil
}

// connectAndPoll waits until Telegram answers getMe, then polls until stopped.
func (c *consumer) connectAndPoll(ctx context.Context) {
	err := retryUntil(ctx, connectInitialDelay, connectMaxDelay, c.fetchMe, func(err error, delay time.Duration) {
		c.logger.Warn("Telegram is unreachable, will retry", zap.Error(err), zap.Duration("retry_in", delay))
	})
	if err != nil {
		return
	}
	c.logger.Info("Telegram connected")

	c.mu.Lock()
	if ctx.Err() != nil {
		c.mu.Unlock()
		return
	}
	c.polling = true
	c.mu.Unlock()

	c.bot.Start()
}

// fetchMe asks Telegram who the bot is and stores the answer, which telebot
// would otherwise have fetched while constructing an online bot.
func (c *consumer) fetchMe() error {
	data, err := c.bot.Raw("getMe", nil)
	if err != nil {
		return fmt.Errorf("failed to call getMe: %w", err)
	}
	var resp struct {
		Result *telebot.User `json:"result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("failed to decode getMe: %w", err)
	}
	if resp.Result == nil {
		return errors.New("getMe returned no bot user")
	}
	c.bot.Me = resp.Result

	return nil
}

func (c *consumer) handleCommands(ctx context.Context, handlers map[string]Handler, tctx telebot.Context) error {
	if !c.permitted(tctx) {
		return nil
	}
	name, ok := route.CommandName(tctx.Text(), c.username())
	if !ok {
		return nil
	}
	if handler, ok := handlers[name]; ok {
		return c.handleCommand(ctx, handler, tctx)
	}

	return nil
}

// username is the bot's own username, known once getMe has answered.
func (c *consumer) username() string {
	if c.bot == nil || c.bot.Me == nil {
		return ""
	}

	return c.bot.Me.Username
}

func (c *consumer) handleCommand(ctx context.Context, handler Handler, tctx telebot.Context) error {
	if !c.permitted(tctx) {
		return nil
	}
	logger := c.logger.With(zap.String("command", handler.Name()))
	message := tctx.Message()

	origin := originOf(message)

	logger = logger.With(zap.Int64("chat_id", origin.ChatID), zap.Int("thread_id", origin.ThreadID),
		zap.Int64("sender_id", origin.SenderID))
	logger.Debug("Handle command")

	defer func() {
		if p := recover(); p != nil {
			logger.With(zap.Any("panic", p)).Error("Panic in message handler")
		}
	}()

	err := handler.Handle(ctx, origin, tctx.Text())
	if err != nil {
		logger.
			With(zap.Error(err)).
			Error("Error in message handler")
		return fmt.Errorf("failed to handle command: %w", err)
	}

	return nil
}

// originOf describes where a command came from. Telegram sets ThreadID on
// replies in the General topic too, so it counts only on topic messages.
// Channel posts have no sender.
func originOf(message *telebot.Message) route.Origin {
	var origin route.Origin
	if message.Chat != nil {
		origin.ChatID = message.Chat.ID
	}
	if message.Sender != nil {
		origin.SenderID = message.Sender.ID
	}
	if message.TopicMessage {
		origin.ThreadID = message.ThreadID
	}

	return origin
}

// chatKindOf maps a Telegram chat type to the kind the access rule knows.
func chatKindOf(chat *telebot.Chat) (route.ChatKind, bool) {
	switch chat.Type {
	case telebot.ChatChannel, telebot.ChatChannelPrivate:
		return route.ChatChannel, true
	case telebot.ChatPrivate:
		return route.ChatPrivate, true
	case telebot.ChatGroup, telebot.ChatSuperGroup:
		return route.ChatGroup, true
	default:
		return 0, false
	}
}

// permitted reports whether the update may command the bot: a channel or a
// private chat must be allowed, a group must be allowed and the sender too.
// Other updates are dropped silently, with one warning carrying chat and
// sender IDs.
func (c *consumer) permitted(tctx telebot.Context) bool {
	chat := tctx.Chat()
	var chatID, senderID int64
	if message := tctx.Message(); message != nil && message.Sender != nil {
		senderID = message.Sender.ID
	}
	if chat != nil {
		chatID = chat.ID
		if kind, ok := chatKindOf(chat); ok && c.access.Permit(kind, chatID, senderID) {
			return true
		}
	}

	c.logger.Warn("Update that is not allowed was dropped",
		zap.Int64("chat_id", chatID), zap.Int64("sender_id", senderID))

	return false
}
