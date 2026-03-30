package main

import (
	"context"
	"fmt"
	"log"
	"os"
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
	"github.com/Toshik1978/server-bot/pkg/script"
	"github.com/Toshik1978/server-bot/pkg/sms"
	"github.com/Toshik1978/server-bot/pkg/speedtest"
	"github.com/Toshik1978/server-bot/pkg/telegram"
	"github.com/Toshik1978/server-bot/pkg/zte"
)

// Build-time constants.
var (
	Buildstamp = "undefined"
	Commit     = "undefined"
)

func main() {
	options := []fx.Option{
		configuration(),
		commandHandlers(),
		fx.Provide(newApplication),
		fx.Invoke(register),
		fx.ErrorHook(&errorHandler{}),
		fx.WithLogger(func(logger *zap.Logger) fxevent.Logger {
			return &fxevent.ZapLogger{Logger: logger}
		}),
	}
	if os.Getenv("BOT_ZTE_HOST") != "" {
		options = append(options, zteProviders(), zteWorkers())
	}
	fx.New(options...).Run()
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
			fx.Annotate(telegram.NewPublisher, fx.As(new(command.Publisher)), fx.As(new(sms.Publisher))),
		),
	)
}

// zteProviders provides ZTE connection and dependent components for fx.
func zteProviders() fx.Option {
	return fx.Options(
		fx.Provide(
			fx.Annotate(
				newZTEConnection,
				fx.As(new(command.ZTE)),
				fx.As(new(sms.ZTE)),
			),
		),
		fx.Provide(
			fx.Annotate(
				func(logger *zap.Logger, publisher command.Publisher, conn command.ZTE) telegram.Handler {
					return command.NewNetworkCommand(logger, publisher, conn, "5g")
				},
				fx.ResultTags(`group:"command_handler"`),
			),
			fx.Annotate(
				func(logger *zap.Logger, publisher command.Publisher, conn command.ZTE) telegram.Handler {
					return command.NewNetworkCommand(logger, publisher, conn, "4g")
				},
				fx.ResultTags(`group:"command_handler"`),
			),
			fx.Annotate(
				func(logger *zap.Logger, publisher command.Publisher, conn command.ZTE) telegram.Handler {
					return command.NewNetworkCommand(logger, publisher, conn, "3g")
				},
				fx.ResultTags(`group:"command_handler"`),
			),
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
		config.Level = zap.NewAtomicLevelAt(zapcore.WarnLevel)
		config.Encoding = "console"
		config.EncoderConfig.EncodeLevel = zapcore.CapitalLevelEncoder
	}
	return config.Build()
}

type config struct {
	AppEnv        string   `env:"APP_ENV"`
	TelegramToken string   `env:"TELEGRAM_TOKEN"`
	ChatID        int64    `env:"TELEGRAM_CHAT_ID"`
	UnknownScript string   `env:"TELEGRAM_UNKNOWN_SCRIPT"`
	PingHosts     []string `env:"BOT_PING_HOSTS" envSeparator:","`
	NutIP         string   `env:"BOT_NUT_IP"`
	NutName       string   `env:"BOT_NUT_NAME"`
	NutUser       string   `env:"BOT_NUT_USER"`
	NutPassword   string   `env:"BOT_NUT_PASS"`
	NutWarning    float64  `env:"BOT_NUT_WARN"`
	NutError      float64  `env:"BOT_NUT_ERR"`
	ZTEHost       string   `env:"BOT_ZTE_HOST"`
	ZTEPassword   string   `env:"BOT_ZTE_PASS"`
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

// newZTEConnection initializes new ZTE connection.
func newZTEConnection(logger *zap.Logger, cfg *config) (*zte.Connection, error) {
	return zte.NewConnection(logger, cfg.ZTEHost, cfg.ZTEPassword)
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
			fx.Annotate(
				func(logger *zap.Logger, cfg *config) command.Script {
					return script.New(logger, cfg.UnknownScript)
				},
			),
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
			fx.Annotate(
				command.NewUnknownCommand,
				fx.As(new(telegram.Handler)),
				fx.ResultTags(`group:"command_handler"`),
			),
		),
	)
}

// zteWorkers provide ZTE workers in the application.
func zteWorkers() fx.Option {
	return fx.Options(
		fx.Provide(
			fx.Annotate(
				func(logger *zap.Logger, publisher sms.Publisher, conn sms.ZTE, cfg *config) app.Worker {
					return sms.NewWorker(logger, publisher, cfg.ChatID, conn)
				},
				fx.ResultTags(`group:"worker"`),
			),
		),
	)
}

// newApplication initializes application.
func newApplication(lc fx.Lifecycle, p app.ApplicationParams) *app.Application {
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
