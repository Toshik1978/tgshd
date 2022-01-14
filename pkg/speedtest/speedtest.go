package speedtest

import (
	"context"
	"fmt"

	st "github.com/showwin/speedtest-go/speedtest"
	"go.uber.org/zap"
)

type speedtest struct {
	logger *zap.Logger
}

// New instantiate new object to check internet connection speed.
func New(logger *zap.Logger) *speedtest {
	logger.Info("Speedtest object created")
	return &speedtest{
		logger: logger,
	}
}

// Speedtest do speed test.
func (s *speedtest) Speedtest(ctx context.Context) (float64, float64, error) {
	server, err := s.findServer(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to get best server for testing: %w", err)
	}

	if err := server.DownloadTestContext(ctx, true); err != nil {
		return 0, 0, fmt.Errorf("failed to do download test: %w", err)
	}
	if err := server.UploadTestContext(ctx, true); err != nil {
		return 0, 0, fmt.Errorf("failed to do upload test: %w", err)
	}

	return server.DLSpeed, server.ULSpeed, nil
}

func (s *speedtest) findServer(ctx context.Context) (*st.Server, error) {
	user, err := st.FetchUserInfoContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch user info: %w", err)
	}
	servers, err := st.FetchServerListContext(ctx, user)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch servers list: %w", err)
	}
	targets, err := servers.FindServer([]int{})
	if err != nil {
		return nil, fmt.Errorf("failed to get server for testing: %w", err)
	}
	if len(targets) != 1 {
		return nil, fmt.Errorf("failed to get server for testing: %w", err)
	}
	return targets[0], nil
}
