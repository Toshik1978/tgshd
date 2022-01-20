package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"time"

	"github.com/caarlos0/env/v6"
	"github.com/go-playground/webhooks/v6/gitlab"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	_ "github.com/jackc/pgx/v4/stdlib"
	"github.com/jmoiron/sqlx"
	_ "github.com/joho/godotenv/autoload"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/Toshik1978/server-bot/cmd/app"
	"github.com/Toshik1978/server-bot/pkg/command"
	"github.com/Toshik1978/server-bot/pkg/ping"
	"github.com/Toshik1978/server-bot/pkg/power"
	"github.com/Toshik1978/server-bot/pkg/sms"
	"github.com/Toshik1978/server-bot/pkg/sms/gammu"
	"github.com/Toshik1978/server-bot/pkg/speedtest"
	"github.com/Toshik1978/server-bot/pkg/telegram"
	"github.com/Toshik1978/server-bot/pkg/webhook"
)

// Build-time constants.
var (
	Buildstamp = "undefined"
	Commit     = "undefined"
)

func main() {
	rand.Seed(time.Now().UTC().UnixNano())

	fxApp := fx.New(
		configuration(),
		commandHandlers(),
		webHandlers(),
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
			newDB,
		),
		fx.Provide(
			newTelegramBot,
			fx.Annotate(telegram.NewConsumer, fx.As(new(telegram.Consumer))),
			fx.Annotate(telegram.NewPublisher, fx.As(new(command.Publisher)), fx.As(new(webhook.Publisher))),
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
	AppEnv            string   `env:"APP_ENV"`
	TelegramToken     string   `env:"TELEGRAM_TOKEN"`
	AlertChatID       int64    `env:"TELEGRAM_CHAT_ID"`
	InternetHosts     []string `env:"BOT_INTERNET_CHECK_HOSTS" envSeparator:","`
	PingHosts         []string `env:"BOT_PING_HOSTS" envSeparator:","`
	NutIP             string   `env:"BOT_NUT_IP"`
	NutName           string   `env:"BOT_NUT_NAME"`
	NutUser           string   `env:"BOT_NUT_USER"`
	NutPassword       string   `env:"BOT_NUT_PASS"`
	NutWarning        float64  `env:"BOT_NUT_WARN"`
	NutError          float64  `env:"BOT_NUT_ERR"`
	GammuDBConnection string   `env:"GAMMU_DB_CONNECTION"`
	WebAddress        string   `env:"WEB_ADDR"`
	GitlabChatID      int64    `env:"TELEGRAM_GITLAB_CHAT_ID"`
	GitlabToken       string   `env:"GITLAB_TOKEN"`
}

// newConfig initializes configuration.
func newConfig() (*config, error) {
	cfg := config{}
	if err := env.Parse(&cfg); err != nil {
		return nil, fmt.Errorf("failed to parse environment: %w", err)
	}
	return &cfg, nil
}

// newDB initializes DB.
func newDB(cfg *config) (*sqlx.DB, error) {
	return sqlx.Open("pgx", cfg.GammuDBConnection)
}

// newTelegramBot initializes new telegram bot.
func newTelegramBot(cfg *config) (*tgbotapi.BotAPI, error) {
	return tgbotapi.NewBotAPI(cfg.TelegramToken)
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
				command.NewService,
				fx.As(new(telegram.Callback)),
				fx.ParamTags(``, `group:"command_handler"`),
			),
		),
		fx.Provide(
			fx.Annotate(
				func(logger *zap.Logger, publisher command.Publisher, pinger command.Pinger, cfg *config) command.Handler {
					return command.NewPingCommand(logger, publisher, pinger, cfg.PingHosts)
				},
				fx.ResultTags(`group:"command_handler"`),
			),
			fx.Annotate(
				command.NewPowerCommand,
				fx.As(new(command.Handler)),
				fx.ResultTags(`group:"command_handler"`),
			),
			fx.Annotate(
				command.NewSpeedCommand,
				fx.As(new(command.Handler)),
				fx.ResultTags(`group:"command_handler"`),
			),
		),
	)
}

// webHandlers provides web handlers information for fx.
func webHandlers() fx.Option {
	return fx.Options(
		fx.Provide(
			func(cfg *config) (*gitlab.Webhook, error) {
				return gitlab.New(gitlab.Options.Secret(cfg.GitlabToken))
			},
			fx.Annotate(
				func(logger *zap.Logger, hook *gitlab.Webhook, publisher webhook.Publisher, cfg *config) app.WebHandler {
					return webhook.NewService(logger, hook, publisher, cfg.GitlabChatID)
				},
				fx.ResultTags(`group:"web_handler"`),
			),
		),
		fx.Provide(
			fx.Annotate(gammu.NewSQLBackend, fx.As(new(sms.Publisher))),
			fx.Annotate(sms.NewService, fx.As(new(app.WebHandler)), fx.ResultTags(`group:"web_handler"`)),
		),
	)
}

// newApplication initializes application.
func newApplication(lc fx.Lifecycle, p app.ApplicationParams, cfg *config) *app.Application {
	a := app.NewApplication(p, cfg.WebAddress, Commit, Buildstamp)
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

type registerParams struct {
	fx.In

	App      *app.Application
	Handlers []app.WebHandler `group:"web_handler"`
}

// register bootstrap application.
func register(params registerParams) {
	params.App.Register(params.Handlers)
}

// errorHandler is an error handler for fx.
type errorHandler struct {
}

func (h *errorHandler) HandleError(err error) {
	log.Print("Runtime FX error: ", err)
}
