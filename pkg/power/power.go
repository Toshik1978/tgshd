package power

import (
	"context"
	"errors"
	"fmt"

	nut "github.com/robbiet480/go.nut"
	"go.uber.org/zap"
)

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
func (p *power) Voltage(_ context.Context) (float64, error) {
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
			return variable.Value.(float64), nil
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
