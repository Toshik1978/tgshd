package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/caarlos0/env/v11"
	_ "github.com/joho/godotenv/autoload"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/telebot.v4"

	"github.com/Toshik1978/server-bot/cmd/app"
	"github.com/Toshik1978/server-bot/pkg/command"
	"github.com/Toshik1978/server-bot/pkg/ping"
	"github.com/Toshik1978/server-bot/pkg/power"
	"github.com/Toshik1978/server-bot/pkg/speedtest"
	"github.com/Toshik1978/server-bot/pkg/telegram"
)

// Build-time constants.
var (
	Buildstamp = "undefined"
	Commit     = "undefined"
)

func main() {
	fxApp := fx.New(
		configuration(),
		commandHandlers(),
		fx.Provide(newApplication),
		fx.Invoke(register),
		fx.ErrorHook(&errorHandler{}),
		fx.WithLogger(func(logger *zap.Logger) fxevent.Logger {
			return &fxevent.ZapLogger{Logger: logger}
		}),
	)
	fxApp.Run()
}

// configuration provides basic configuration for fx.
func configuration() fx.Option {
	return fx.Options(
		fx.Provide(
			newLogger,
			newConfig,
		),
		fx.Provide(
			newTelegramBot,
			fx.Annotate(
				telegram.NewConsumer,
				fx.As(new(telegram.Consumer)),
				fx.ParamTags(``, ``, `group:"command_handler"`),
			),
			fx.Annotate(telegram.NewPublisher, fx.As(new(command.Publisher))),
		),
	)
}

// newLogger initializes logger for console.
func newLogger(cfg *config) (*zap.Logger, error) {
	var config zap.Config
	if cfg.AppEnv == "development" {
		config = zap.NewDevelopmentConfig()
		config.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	} else {
		config = zap.NewProductionConfig()
		config.Encoding = "console"
		config.EncoderConfig.EncodeLevel = zapcore.CapitalLevelEncoder
	}
	return config.Build()
}

type config struct {
	AppEnv        string   `env:"APP_ENV"`
	TelegramToken string   `env:"TELEGRAM_TOKEN"`
	AlertChatID   int64    `env:"TELEGRAM_CHAT_ID"`
	PingHosts     []string `env:"BOT_PING_HOSTS" envSeparator:","`
	NutIP         string   `env:"BOT_NUT_IP"`
	NutName       string   `env:"BOT_NUT_NAME"`
	NutUser       string   `env:"BOT_NUT_USER"`
	NutPassword   string   `env:"BOT_NUT_PASS"`
	NutWarning    float64  `env:"BOT_NUT_WARN"`
	NutError      float64  `env:"BOT_NUT_ERR"`
}

// newConfig initializes configuration.
func newConfig() (*config, error) {
	cfg := config{}
	if err := env.Parse(&cfg); err != nil {
		return nil, fmt.Errorf("failed to parse environment: %w", err)
	}
	return &cfg, nil
}

// newTelegramBot initializes new telegram bot.
func newTelegramBot(cfg *config) (*telebot.Bot, error) {
	pref := telebot.Settings{
		Token:  cfg.TelegramToken,
		Poller: &telebot.LongPoller{Timeout: 5 * time.Second},
	}
	return telebot.NewBot(pref)
}

// commandHandlers provides telegram commands information for fx.
func commandHandlers() fx.Option {
	return fx.Options(
		fx.Provide(
			fx.Annotate(
				func(logger *zap.Logger, cfg *config) command.Power {
					return power.New(logger, cfg.NutIP, cfg.NutName, cfg.NutUser, cfg.NutPassword)
				},
			),
			fx.Annotate(speedtest.New, fx.As(new(command.Speedtest))),
			fx.Annotate(ping.New, fx.As(new(command.Pinger))),
		),
		fx.Provide(
			fx.Annotate(
				func(logger *zap.Logger, publisher command.Publisher, pinger command.Pinger, cfg *config) telegram.Handler {
					return command.NewPingCommand(logger, publisher, pinger, cfg.PingHosts)
				},
				fx.ResultTags(`group:"command_handler"`),
			),
			fx.Annotate(
				command.NewPowerCommand,
				fx.As(new(telegram.Handler)),
				fx.ResultTags(`group:"command_handler"`),
			),
			fx.Annotate(
				command.NewSpeedCommand,
				fx.As(new(telegram.Handler)),
				fx.ResultTags(`group:"command_handler"`),
			),
		),
	)
}

// newApplication initializes application.
func newApplication(lc fx.Lifecycle, p app.ApplicationParams, cfg *config) *app.Application {
	a := app.NewApplication(p, Commit, Buildstamp)
	cancelCtx, cancel := context.WithCancel(context.Background())

	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			return a.OnStart(cancelCtx)
		},
		OnStop: func(ctx context.Context) error {
			return a.OnStop(ctx, cancel)
		},
	})
	return a
}

// register bootstrap application.
func register(a *app.Application) {
	a.Bootstrap()
}

// errorHandler is an error handler for fx.
type errorHandler struct {
}

func (h *errorHandler) HandleError(err error) {
	log.Print("Runtime FX error: ", err)
}
