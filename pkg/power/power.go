package power

import (
	"context"
	"errors"
	"fmt"
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

	type result struct {
		voltage float64
		err     error
	}
	// Buffered so the goroutine never blocks on send even after we return on
	// ctx.Done(). go.nut has no cancellation support, so the read runs to
	// completion in the background and its result is discarded on timeout.
	ch := make(chan result, 1)
	go func() {
		voltage, err := p.readVoltage()
		ch <- result{voltage: voltage, err: err}
	}()

	select {
	case <-ctx.Done():
		return 0, fmt.Errorf("failed to read ups voltage: %w", ctx.Err())
	case r := <-ch:
		return r.voltage, r.err
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
