// Package docker is a thin adapter over the Docker Engine API. It exposes only
// the fields the health checks need and never labels, environment, command
// lines or raw event attributes, which can carry secrets.
package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// callTimeout bounds every Engine API call so a hung daemon cannot stall a cycle.
const callTimeout = 10 * time.Second

// ErrNotFound is wrapped by Inspect when the named container does not exist.
var ErrNotFound = errors.New("container not found")

// Container is the safe subset of container state.
type Container struct {
	ID            string // full 64-hex id, as in the docker-<id>.scope cgroup name
	Name          string
	RestartPolicy string
	Running       bool
	Status        string
	Health        string
	RestartCount  int
	StartedAt     time.Time
	MemoryLimit   int64 // HostConfig.Memory in bytes; 0 means no limit
}

// engine is the subset of the Engine API client the adapter uses.
type engine interface {
	ContainerList(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error)
	ContainerInspect(
		ctx context.Context, id string, options client.ContainerInspectOptions,
	) (client.ContainerInspectResult, error)
	ContainerRestart(
		ctx context.Context, id string, options client.ContainerRestartOptions,
	) (client.ContainerRestartResult, error)
	ContainerStop(
		ctx context.Context, id string, options client.ContainerStopOptions,
	) (client.ContainerStopResult, error)
	Close() error
}

// Client talks to the Docker daemon.
type Client struct {
	api engine
}

// New builds a client from DOCKER_HOST (or the default socket). It does not
// connect, so it succeeds when Docker is absent.
func New() (*Client, error) {
	api, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}

	return &Client{api: api}, nil
}

// Close releases the client's resources.
func (c *Client) Close() error {
	if err := c.api.Close(); err != nil {
		return fmt.Errorf("close docker client: %w", err)
	}

	return nil
}

// List returns all containers (running or not) with their inspected state.
// Containers removed between the listing and their inspection are skipped.
func (c *Client) List(ctx context.Context) ([]Container, error) {
	listCtx, cancel := context.WithTimeout(ctx, callTimeout)
	list, err := c.api.ContainerList(listCtx, client.ContainerListOptions{All: true})
	cancel()
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}

	result := make([]Container, 0, len(list.Items))
	for i := range list.Items {
		inspected, found, err := c.inspect(ctx, list.Items[i].ID)
		if err != nil {
			return nil, err
		}
		if found {
			result = append(result, mapContainer(inspected))
		}
	}

	return result, nil
}

// Inspect returns the state of the named container. When the inspect fails and
// a re-list filtered by name finds no such container, the error wraps
// ErrNotFound; any other failure is returned as it is.
func (c *Client) Inspect(ctx context.Context, name string) (Container, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	res, err := c.api.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if err != nil {
		if c.missing(ctx, name) {
			return Container{}, fmt.Errorf("inspect container %q: %w: %w", name, ErrNotFound, err)
		}

		return Container{}, fmt.Errorf("inspect container %q: %w", name, err)
	}

	return mapContainer(res.Container), nil
}

// Restart restarts the named container.
func (c *Client) Restart(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*callTimeout)
	defer cancel()

	if _, err := c.api.ContainerRestart(ctx, name, client.ContainerRestartOptions{}); err != nil {
		return fmt.Errorf("restart container %q: %w", name, err)
	}

	return nil
}

// Stop stops the named container.
func (c *Client) Stop(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*callTimeout)
	defer cancel()

	if _, err := c.api.ContainerStop(ctx, name, client.ContainerStopOptions{}); err != nil {
		return fmt.Errorf("stop container %q: %w", name, err)
	}

	return nil
}

// missing reports whether a list filtered by name proves the container does
// not exist. The name filter matches substrings, so the names are compared
// exactly. A failing re-list proves nothing.
func (c *Client) missing(ctx context.Context, name string) bool {
	list, err := c.api.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: client.Filters{}.Add("name", name),
	})
	if err != nil {
		return false
	}

	for i := range list.Items {
		for _, n := range list.Items[i].Names {
			if strings.TrimPrefix(n, "/") == name {
				return false
			}
		}
	}

	return true
}

// inspect inspects one container under its own timeout. On failure it re-lists
// by id: if the container is gone it reports found=false, otherwise it returns
// the original error.
func (c *Client) inspect(ctx context.Context, id string) (container.InspectResponse, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	res, err := c.api.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err == nil {
		return res.Container, true, nil
	}

	again, listErr := c.api.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: client.Filters{}.Add("id", id),
	})
	if listErr == nil && len(again.Items) == 0 {
		return container.InspectResponse{}, false, nil
	}

	return container.InspectResponse{}, false, fmt.Errorf("inspect container: %w", err)
}

// mapContainer copies the allowed fields out of an inspect response.
func mapContainer(in container.InspectResponse) Container {
	out := Container{
		ID:           in.ID,
		Name:         strings.TrimPrefix(in.Name, "/"),
		RestartCount: in.RestartCount,
	}
	if in.HostConfig != nil {
		out.RestartPolicy = string(in.HostConfig.RestartPolicy.Name)
		out.MemoryLimit = in.HostConfig.Memory
	}
	if in.State != nil {
		out.Running = in.State.Running
		out.Status = string(in.State.Status)
		if t, err := time.Parse(time.RFC3339Nano, in.State.StartedAt); err == nil {
			out.StartedAt = t
		}
		if in.State.Health != nil {
			out.Health = string(in.State.Health.Status)
		}
	}

	return out
}
