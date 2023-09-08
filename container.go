package docker

import (
	"context"
	"fmt"
	"github.com/docker/docker/api/types/container"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
)

type connectorContainer struct {
	id               string
	hijackedResponse *types.HijackedResponse
	cli              *client.Client

	multiplexedReader
}

func (c connectorContainer) Write(p []byte) (n int, err error) {
	return c.hijackedResponse.Conn.Write(p)
}

func (c connectorContainer) Close() error {
	if c.hijackedResponse != nil {
		c.hijackedResponse.Close()
		_ = c.hijackedResponse.CloseWrite()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	/*if err := c.cli.ContainerRemove(ctx, c.id, types.ContainerRemoveOptions{
		Force: true,
	}); err != nil {
		if !client.IsErrNotFound(err) {
			return fmt.Errorf("failed to remove container %s (%w)", c.id, err)
		}
	}*/
	err := c.cli.ContainerStop(ctx, c.id, container.StopOptions{})
	if err != nil {
		if !client.IsErrNotFound(err) {
			return fmt.Errorf("failed to stop container %s (%w)", c.id, err)
		}
	}

	return nil
}

func (c connectorContainer) ID() string {
	return c.id
}
