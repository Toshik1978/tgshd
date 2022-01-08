package command

import (
	"context"
	"fmt"

	nut "github.com/robbiet480/go.nut"
	"go.uber.org/zap"
)

type powerCommand struct {
	logger    *zap.Logger
	publisher Publisher
	ip        string
	name      string
	user      string
	password  string
}

// NewPowerCommand creates new handler for power command.
func NewPowerCommand(
	logger *zap.Logger,
	publisher Publisher,
	ip, name, user, password string,
) *powerCommand {
	logger.Info("Power command created")
	return &powerCommand{
		logger:    logger,
		publisher: publisher,
		ip:        ip,
		name:      name,
		user:      user,
		password:  password,
	}
}

func (c *powerCommand) Name() string {
	return "power"
}

func (c *powerCommand) Handle(ctx context.Context, senderID int64) error {
	ups, client, err := c.connect()
	if err != nil {
		return fmt.Errorf("failed connect to ups: %w", err)
	}
	defer c.disconnect(client)

	vars, err := ups.GetVariables()
	if err != nil {
		return fmt.Errorf("failed to get nut variables: %w", err)
	}

	text := "Voltage: unknown"
	for _, variable := range vars {
		if variable.Name == "input.voltage" {
			value := variable.Value.(float64)
			text = fmt.Sprintf("Voltage: %.1f", value)
		}
	}

	if err := c.publisher.Publish(ctx, senderID, text); err != nil {
		return fmt.Errorf("failed to publish reply: %w", err)
	}
	return nil
}

func (c *powerCommand) connect() (*nut.UPS, *nut.Client, error) {
	client, err := nut.Connect(c.ip)
	if err != nil {
		return nil, nil, fmt.Errorf("failed connect to nut: %w", err)
	}
	ok, err := client.Authenticate(c.user, c.password)
	if err != nil {
		return nil, nil, fmt.Errorf("failed authenticate in nut: %w", err)
	}
	if !ok {
		return nil, nil, fmt.Errorf("failed authenticate in nut")
	}
	ups, err := nut.NewUPS(c.name, &client)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to initialize ups: %w", err)
	}
	return &ups, &client, nil
}

func (c *powerCommand) disconnect(client *nut.Client) {
	_, _ = client.Disconnect()
}
