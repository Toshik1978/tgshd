package script

import (
	"context"
	"fmt"
	"os/exec"

	"go.uber.org/zap"
)

type script struct {
	logger     *zap.Logger
	scriptFile string
}

// New instantiate script object.
func New(logger *zap.Logger, scriptFile string) *script {
	logger.Info("Script object created")
	return &script{
		logger:     logger,
		scriptFile: scriptFile,
	}
}

// Name runs the script to get the list of available commands.
func (s *script) Name() (string, error) {
	if s.scriptFile == "" {
		return "", nil
	}

	stream, err := exec.Command(s.scriptFile).Output() //nolint:gosec
	if err != nil {
		s.logger.Error("Failed to execute script", zap.Error(err))
		return "", fmt.Errorf("failed to execute script reply: %w", err)
	}

	return string(stream), nil
}

// Execute runs the script.
func (s *script) Execute(ctx context.Context, cmd string) error {
	if s.scriptFile == "" {
		return nil
	}

	command := exec.CommandContext(ctx, s.scriptFile, cmd) //nolint:gosec
	if err := command.Run(); err != nil {
		s.logger.Error("Failed to execute script", zap.Error(err))
		return fmt.Errorf("failed to execute script: %w", err)
	}

	return nil
}
