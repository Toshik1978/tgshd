package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/caarlos0/env/v11"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/joho/godotenv/autoload"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/telebot.v4"

	"github.com/Toshik1978/tgshd/cmd/app"
	"github.com/Toshik1978/tgshd/pkg/command"
	"github.com/Toshik1978/tgshd/pkg/mail"
	"github.com/Toshik1978/tgshd/pkg/ping"
	"github.com/Toshik1978/tgshd/pkg/power"
	"github.com/Toshik1978/tgshd/pkg/script"
	"github.com/Toshik1978/tgshd/pkg/sms"
	"github.com/Toshik1978/tgshd/pkg/sms/gammu"
	"github.com/Toshik1978/tgshd/pkg/speedtest"
	"github.com/Toshik1978/tgshd/pkg/telegram"
	"github.com/Toshik1978/tgshd/pkg/zte"
)

// Build-time constants.
//
//nolint:gochecknoglobals // injected via -ldflags at build time; must be package-level vars.
var (
	Buildstamp = "undefined"
	Commit     = "undefined"
)

// telegramTimeout bounds one Telegram Bot API request (telebot's default).
const telegramTimeout = time.Minute

// daemonStopTimeout bounds the daemon's whole stop: fx runs every OnStop hook
// under it. In stop order it covers the scheduler waiting for a running tick
// (app.SchedulerStopTimeout), the memory guard's last outcome reports
// (memguard.StopReportTimeout) and records write (guardWriteTimeout), and the
// final health checkpoint (up to runner.InterruptedSaveTimeout of grace), with
// room to spare. The systemd unit's TimeoutStopSec and a container's stop
// grace period must exceed it (README, "Stopping").
const daemonStopTimeout = 40 * time.Second

func main() {
	// A subcommand (contracts/cli.md) runs instead of the daemon.
	cio := cliIO{
		getenv: os.Getenv, stdout: os.Stdout, stderr: os.Stderr, now: time.Now,
		checkProviders: healthCheckProviders(),
	}
	if code, ok := runSubcommand(os.Args[1:], cio); ok {
		os.Exit(code)
	}

	fx.New(appOptions(os.Getenv)...).Run()
}

// appOptions assembles the fx options; optional features are gated on the
// environment read through getenv.
func appOptions(getenv func(string) string) []fx.Option {
	options := []fx.Option{
		configuration(),
		commandHandlers(),
		fx.Provide(newApplication),
		// Force construction of *Application so its lifecycle hook registers.
		fx.Invoke(func(*app.Application) {}),
		fx.ErrorHook(&errorHandler{}),
		fx.StopTimeout(daemonStopTimeout),
		fx.WithLogger(func(logger *zap.Logger) fxevent.Logger {
			return &fxevent.ZapLogger{Logger: logger}
		}),
	}
	if getenv("BOT_ZTE_HOST") != "" {
		options = append(options, zteProviders(), zteWorkers())
	}
	if getenv("BOT_GAMMU_DSN") != "" {
		options = append(options, gammuProviders(), gammuInboxProviders())
	}
	if getenv("BOT_HEALTH_CONFIG") != "" {
		options = append(options, healthProviders())
	}

	return options
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
			newAccessList,
			fx.Annotate(
				telegram.NewConsumer,
				fx.As(new(app.TelegramConsumer)),
				fx.ParamTags(``, ``, ``, `group:"command_handler"`),
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

// gammuProviders provides the gammu SMS backend and the /sms command for fx.
func gammuProviders() fx.Option {
	return fx.Options(
		fx.Provide(
			newGammuDB,
			fx.Annotate(gammu.NewSQLBackend, fx.As(new(command.SmsSender))),
		),
		fx.Provide(
			fx.Annotate(
				func(
					logger *zap.Logger,
					publisher command.Publisher,
					sender command.SmsSender,
					cfg *config,
				) telegram.Handler {
					return command.NewSmsCommand(logger, publisher, sender, cfg.ChatID)
				},
				fx.ResultTags(`group:"command_handler"`),
			),
		),
	)
}

// newGammuDB opens the gammu Postgres database and closes it on shutdown.
func newGammuDB(lc fx.Lifecycle, cfg *config) (*sql.DB, error) {
	db, err := sql.Open("pgx", cfg.GammuDSN)
	if err != nil {
		return nil, fmt.Errorf("failed to open gammu database: %w", err)
	}

	// Bound the pool: SMS traffic is low-volume, so a small cap plus a
	// lifetime ceiling keeps stale Postgres connections from lingering in a
	// long-running daemon.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)

	lc.Append(fx.Hook{
		OnStop: func(_ context.Context) error {
			return db.Close()
		},
	})

	return db, nil
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

	logger, err := config.Build()
	if err != nil {
		return nil, fmt.Errorf("failed to build logger: %w", err)
	}

	return logger, nil
}

// newOneShotLogger builds the logger for `check --once`: errors only, so
// adapter warnings do not interleave with the verdict lines on the output.
func newOneShotLogger(cfg *config) (*zap.Logger, error) {
	config := zap.NewProductionConfig()
	config.Level = zap.NewAtomicLevelAt(zapcore.ErrorLevel)
	config.Encoding = "console"
	config.EncoderConfig.EncodeLevel = zapcore.CapitalLevelEncoder
	if cfg.AppEnv == "development" {
		config.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	}

	logger, err := config.Build()
	if err != nil {
		return nil, fmt.Errorf("failed to build logger: %w", err)
	}

	return logger, nil
}

type config struct {
	AppEnv        string   `env:"APP_ENV"`
	TelegramToken string   `env:"TELEGRAM_TOKEN"`
	ChatID        int64    `env:"TELEGRAM_CHAT_ID"`
	AllowedIDs    []int64  `env:"TELEGRAM_ALLOWED_IDS"    envSeparator:","`
	UnknownScript string   `env:"TELEGRAM_UNKNOWN_SCRIPT"`
	PingHosts     []string `env:"BOT_PING_HOSTS"          envSeparator:","`
	NutIP         string   `env:"BOT_NUT_IP"`
	NutName       string   `env:"BOT_NUT_NAME"`
	NutUser       string   `env:"BOT_NUT_USER"`
	NutPassword   string   `env:"BOT_NUT_PASS"`
	NutWarning    float64  `env:"BOT_NUT_WARN"`
	NutError      float64  `env:"BOT_NUT_ERR"`
	ZTEHost       string   `env:"BOT_ZTE_HOST"`
	ZTEPassword   string   `env:"BOT_ZTE_PASS"`
	GammuDSN      string   `env:"BOT_GAMMU_DSN"`
	HealthConfig  string   `env:"BOT_HEALTH_CONFIG"`
	HostName      string   `env:"BOT_HOST_NAME"`
	SMSPhone      string   `env:"BOT_SMS_PHONE"`
	SMTP          mail.Config
}

// newConfig initializes configuration.
func newConfig() (*config, error) {
	cfg := config{}
	if err := env.Parse(&cfg); err != nil {
		return nil, fmt.Errorf("failed to parse environment: %w", err)
	}
	return &cfg, nil
}

// newAccessList builds the Telegram allow-list from the configuration.
func newAccessList(cfg *config) *telegram.AccessList {
	return telegram.NewAccessList(cfg.ChatID, cfg.AllowedIDs)
}

// newTelegramBot initializes new telegram bot.
func newTelegramBot(cfg *config) (*telebot.Bot, error) {
	pref := telebot.Settings{
		Token:  cfg.TelegramToken,
		Poller: &telebot.LongPoller{Timeout: 5 * time.Second},
		// telebot takes no context, so this bounds every Bot API call,
		// including health messages sent with a deadline-less context.
		Client: &http.Client{Timeout: telegramTimeout},
		// Do not call getMe while constructing: with no network at boot that
		// would stop the daemon and health monitoring with it. The consumer
		// connects in the background instead.
		Offline: true,
	}

	bot, err := telebot.NewBot(pref)
	if err != nil {
		return nil, fmt.Errorf("failed to create telegram bot: %w", err)
	}

	return bot, nil
}

// newZTEConnection initializes new ZTE connection.
func newZTEConnection(logger *zap.Logger, cfg *config) (*zte.Connection, error) {
	conn, err := zte.NewConnection(logger, cfg.ZTEHost, cfg.ZTEPassword)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to zte modem: %w", err)
	}

	return conn, nil
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
				func(
					logger *zap.Logger,
					publisher command.Publisher,
					pinger command.Pinger,
					cfg *config,
				) telegram.Handler {
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

// errorHandler is an error handler for fx.
type errorHandler struct{}

func (h *errorHandler) HandleError(err error) {
	log.Print("Runtime FX error: ", err)
}
