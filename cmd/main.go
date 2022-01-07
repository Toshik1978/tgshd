package main

import (
	"context"
	"fmt"
	"log"

	"github.com/caarlos0/env/v6"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	_ "github.com/joho/godotenv/autoload"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/Toshik1978/server-bot/cmd/app"
	"github.com/Toshik1978/server-bot/pkg/command"
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
			fx.Annotate(telegram.NewConsumer, fx.As(new(telegram.Consumer))),
			fx.Annotate(telegram.NewPublisher, fx.As(new(telegram.Publisher))),
		),
	)
}

// commandHandlers provides telegram commands information for fx.
func commandHandlers() fx.Option {
	return fx.Options(
		fx.Provide(
			fx.Annotate(
				command.NewService,
				fx.As(new(telegram.Callback)),
				fx.ParamTags(``, ``, `group:"command_handler"`),
			),
		),
		fx.Provide(
			fx.Annotate(newPingCommand, fx.ResultTags(`group:"command_handler"`)),
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
	}
	return config.Build()
}

type config struct {
	AppEnv        string   `env:"APP_ENV"`
	TelegramToken string   `env:"TELEGRAM_BOT_TOKEN"`
	PingHosts     []string `env:"PING_HOSTS" envSeparator:","`
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
func newTelegramBot(cfg *config) (*tgbotapi.BotAPI, error) {
	return tgbotapi.NewBotAPI(cfg.TelegramToken)
}

// newPingCommand initializes new ping command.
func newPingCommand(logger *zap.Logger, publisher telegram.Publisher, cfg *config) command.Handler {
	return command.NewPingCommand(logger, publisher, cfg.PingHosts)
}

// newApplication initializes application.
func newApplication(lc fx.Lifecycle, p app.ApplicationParams, _ *config) *app.Application {
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
func register(application *app.Application) {
	application.Bootstrap()
}

// errorHandler is an error handler for fx.
type errorHandler struct {
}

func (h *errorHandler) HandleError(err error) {
	log.Print("Runtime FX error: ", err)
}
