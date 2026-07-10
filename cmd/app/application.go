package app

import (
	"context"
	"fmt"

	"github.com/go-co-op/gocron/v2"
	"go.uber.org/fx"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// ApplicationParams declare parameters to run application.
type ApplicationParams struct {
	fx.In

	Logger   *zap.Logger
	Telegram TelegramConsumer
	Workers  []Worker `group:"worker"`
}

// Application declare new instance of application.
type Application struct {
	logger     *zap.Logger
	tlg        TelegramConsumer
	workers    []Worker
	scheduler  gocron.Scheduler
	commit     string
	buildstamp string
}

// NewApplication creates new instance of application.
func NewApplication(p ApplicationParams, commit, buildstamp string) *Application {
	return &Application{
		logger:     p.Logger,
		tlg:        p.Telegram,
		workers:    p.Workers,
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

func (a *Application) Bootstrap() {
}

func (a *Application) onStart(ctx context.Context) error {
	scheduler, err := gocron.NewScheduler()
	if err != nil {
		return fmt.Errorf("failed to start scheduler: %w", err)
	}

	a.scheduler = scheduler
	for _, worker := range a.workers {
		job, err := a.scheduler.
			NewJob(
				gocron.DurationJob(worker.Duration()),
				gocron.NewTask(
					func(worker Worker) {
						a.logger.
							With(zap.String("worker_name", worker.Name())).
							Info("Run worker")

						if err := worker.Do(ctx); err != nil {
							a.logger.
								With(zap.String("worker_name", worker.Name())).
								With(zap.Error(err)).
								Error("Failed to run worker")
						}
					},
					worker,
				),
				gocron.WithSingletonMode(gocron.LimitModeReschedule),
			)
		if err != nil {
			return fmt.Errorf("failed to start worker: %w", err)
		}

		a.logger.
			With(zap.String("name", worker.Name())).
			With(zap.String("job_id", job.ID().String())).
			Info("Worker scheduled")
	}
	a.scheduler.Start()
	a.logger.Info("Scheduler started")

	if err := a.tlg.Start(ctx); err != nil {
		return fmt.Errorf("failed to start telegram handler: %w", err)
	}

	return nil
}

func (a *Application) onStop(ctx context.Context) error {
	grp, grpCtx := errgroup.WithContext(ctx)
	grp.Go(func() error { return a.tlg.Stop(grpCtx) })
	grp.Go(func() error {
		_ = a.scheduler.Shutdown()
		a.logger.Info("Scheduler stopped")

		return nil
	})

	if err := grp.Wait(); err != nil {
		return fmt.Errorf("failed to stop application components: %w", err)
	}

	return nil
}
