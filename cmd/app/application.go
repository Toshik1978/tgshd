package app

import (
	"context"
	"fmt"

	"go.uber.org/fx"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"github.com/Toshik1978/server-bot/pkg/telegram"
)

// ApplicationParams declare parameters to run application.
type ApplicationParams struct {
	fx.In

	Logger   *zap.Logger
	Telegram telegram.Consumer
}

// Application declare new instance of application.
type Application struct {
	logger     *zap.Logger
	tlg        telegram.Consumer
	commit     string
	buildstamp string
}

// NewApplication creates new instance of application.
func NewApplication(p ApplicationParams, commit, buildstamp string) *Application {
	return &Application{
		logger:     p.Logger,
		tlg:        p.Telegram,
		commit:     commit,
		buildstamp: buildstamp,
	}
}

// OnStart calls in hook on application start.
func (a *Application) OnStart(ctx context.Context) error {
	logger := a.logger.
		With(zap.String("commit", a.commit)).
		With(zap.String("build_timestamp", a.buildstamp))

	logger.Info("Start application")
	if err := a.onStart(ctx); err != nil {
		logger.With(zap.Error(err)).Error("Failed to start application")
		return fmt.Errorf("failed to start application: %w", err)
	}
	return nil
}

func (a *Application) onStart(ctx context.Context) error {
	if err := a.tlg.Start(ctx); err != nil {
		return fmt.Errorf("failed to start telegram handler: %w", err)
	}
	return nil
}

// OnStop calls in hook on application stop.
func (a *Application) OnStop(ctx context.Context, cancel context.CancelFunc) error {
	defer func() { _ = a.logger.Sync() }()

	logger := a.logger.
		With(zap.String("commit", a.commit)).
		With(zap.String("build_timestamp", a.buildstamp))

	cancel()
	if err := a.onStop(ctx); err != nil {
		logger.With(zap.Error(err)).Error("Failed to stop application")
		return fmt.Errorf("failed to stop application: %w", err)
	}

	logger.Info("Stop application")
	return nil
}

func (a *Application) onStop(ctx context.Context) error {
	grp, grpCtx := errgroup.WithContext(ctx)
	grp.Go(func() error { return a.tlg.Stop(grpCtx) })
	return grp.Wait()
}

func (a *Application) Bootstrap() {
}
