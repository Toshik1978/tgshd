package power

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	nut "github.com/robbiet480/go.nut"
	"go.uber.org/zap"
)

// voltageTimeout bounds a single UPS read. go.nut dials over TCP without a
// deadline, so a black-holed NUT host would otherwise hang the /power handler
// until the OS connect timeout (minutes).
const voltageTimeout = 15 * time.Second

type power struct {
	logger   *zap.Logger
	ip       string
	name     string
	user     string
	password string

	// reader overrides the blocking status read in tests.
	reader func() (Status, error)
	// inflight is set while a go.nut read goroutine is running.
	inflight atomic.Bool
}

// New instantiate power control object.
func New(logger *zap.Logger, ip, name, user, password string) *power {
	logger.Info("Power object created")

	return &power{
		logger:   logger,
		ip:       ip,
		name:     name,
		user:     user,
		password: password,
	}
}

// Valid return true if module is able to detect power.
func (p *power) Valid() bool {
	return p.ip != "" && p.name != "" && p.user != "" && p.password != ""
}

// Voltage retrieve current UPS voltage.
func (p *power) Voltage(ctx context.Context) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, voltageTimeout)
	defer cancel()

	voltage, err := bounded(ctx, p, p.readVoltage)
	if err != nil {
		return 0, fmt.Errorf("failed to read ups voltage: %w", err)
	}

	return voltage, nil
}

// bounded runs a blocking go.nut read so that it honors ctx. go.nut has no
// cancellation or deadline support, so a read against a silent server keeps
// its goroutine and socket until the OS gives up. To keep that bounded to one
// leaked read, a new read is refused with ErrTimeout while a previous one is
// still in flight.
func bounded[T any]( //nolint:ireturn // generic helper shared by Voltage and Status.
	ctx context.Context,
	p *power,
	read func() (T, error),
) (
	T, error,
) {
	var zero T
	if !p.inflight.CompareAndSwap(false, true) {
		return zero, fmt.Errorf("%w: previous read still in flight", ErrTimeout)
	}

	type result struct {
		value T
		err   error
	}
	// Buffered so the goroutine never blocks on send after we return on ctx.Done().
	ch := make(chan result, 1)
	go func() {
		defer p.inflight.Store(false)

		value, err := read()
		ch <- result{value: value, err: err}
	}()

	select {
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return zero, fmt.Errorf("%w: %w", ErrTimeout, ctx.Err())
		}

		return zero, fmt.Errorf("ups read cancelled: %w", ctx.Err())
	case r := <-ch:
		return r.value, r.err
	}
}

// readVoltage performs the blocking NUT read. It has no cancellation because
// go.nut owns the TCP connection; Voltage wraps it to honor ctx.
func (p *power) readVoltage() (float64, error) {
	ups, client, err := p.connect()
	if err != nil {
		return 0, fmt.Errorf("failed connect to ups: %w", err)
	}
	defer p.disconnect(client)

	vars, err := ups.GetVariables()
	if err != nil {
		return 0, fmt.Errorf("failed to get nut variables: %w", err)
	}

	for _, variable := range vars {
		if variable.Name == "input.voltage" {
			voltage, ok := variable.Value.(float64)
			if !ok {
				return 0, errors.New("input.voltage is not a float64")
			}

			return voltage, nil
		}
	}

	return 0, nil
}

func (p *power) connect() (*nut.UPS, *nut.Client, error) {
	client, err := nut.Connect(p.ip)
	if err != nil {
		return nil, nil, fmt.Errorf("failed connect to nut: %w", err)
	}
	ok, err := client.Authenticate(p.user, p.password)
	if err != nil {
		return nil, nil, fmt.Errorf("failed authenticate in nut: %w", err)
	}
	if !ok {
		return nil, nil, errors.New("failed authenticate in nut")
	}
	ups, err := nut.NewUPS(p.name, &client)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to initialize ups: %w", err)
	}

	return &ups, &client, nil
}

func (p *power) disconnect(client *nut.Client) {
	_, _ = client.Disconnect()
}
