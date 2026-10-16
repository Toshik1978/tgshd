package power

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	nut "github.com/robbiet480/go.nut"
)

// statusTimeout bounds a single freshness read when the caller's context has
// no earlier deadline.
const statusTimeout = 15 * time.Second

// Sentinel errors returned by Status. Each one means the UPS monitoring
// service is not delivering a fresh reading.
var (
	// ErrStale means NUT reported DATA-STALE.
	ErrStale = errors.New("ups data is stale")
	// ErrDriverNotConnected means NUT reported DRIVER-NOT-CONNECTED.
	ErrDriverNotConnected = errors.New("ups driver is not connected")
	// ErrConnection means the NUT server could not be reached or refused us.
	ErrConnection = errors.New("ups connection failed")
	// ErrEmpty means the NUT server answered without the expected variables.
	ErrEmpty = errors.New("ups answered with no data")
	// ErrTimeout means the NUT server did not answer in time.
	ErrTimeout = errors.New("ups read timed out")
)

// Status is one fresh UPS reading. Charge and Voltage are nil when the UPS
// does not report them.
type Status struct {
	State   string
	Charge  *float64
	Voltage *float64
}

// Status reads ups.status, battery.charge and input.voltage. The read is
// bounded by ctx and statusTimeout; failures wrap one of the sentinel errors.
func (p *power) Status(ctx context.Context) (Status, error) {
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()

	read := p.reader
	if read == nil {
		read = p.readStatus
	}

	return bounded(ctx, p, read)
}

func (p *power) readStatus() (Status, error) {
	ups, client, err := p.connect()
	if err != nil {
		return Status{}, mapNUTError(err)
	}
	defer p.disconnect(client)

	vars, err := ups.GetVariables()
	if err != nil {
		return Status{}, mapNUTError(err)
	}

	return buildStatus(vars)
}

// mapNUTError turns a go.nut error into one of the sentinel errors.
func mapNUTError(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "DATA-STALE"):
		return fmt.Errorf("%w: %w", ErrStale, err)
	case strings.Contains(msg, "DRIVER-NOT-CONNECTED"):
		return fmt.Errorf("%w: %w", ErrDriverNotConnected, err)
	default:
		return fmt.Errorf("%w: %w", ErrConnection, err)
	}
}

// buildStatus extracts the monitored variables; none present means an empty answer.
func buildStatus(vars []nut.Variable) (Status, error) {
	var status Status
	found := false
	for _, v := range vars {
		switch v.Name {
		case "ups.status":
			status.State = fmt.Sprint(v.Value)
			found = true
		case "battery.charge":
			status.Charge = toFloat(v.Value)
			found = found || status.Charge != nil
		case "input.voltage":
			status.Voltage = toFloat(v.Value)
			found = found || status.Voltage != nil
		}
	}
	if !found {
		return Status{}, ErrEmpty
	}

	return status, nil
}

func toFloat(v any) *float64 {
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case int64:
		f = float64(n)
	case int:
		f = float64(n)
	default:
		return nil
	}

	return &f
}
