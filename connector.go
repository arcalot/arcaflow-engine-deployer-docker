package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	img "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"go.arcalot.io/log/v2"
	"go.flow.arcalot.io/deployer"
)

type connector struct {
	cli    *client.Client
	config *Config
	logger log.Logger
}

func (c *connector) Deploy(ctx context.Context, image string) (deployer.Plugin, error) {
	if err := c.pullImage(ctx, image); err != nil {
		return nil, err
	}

	c.logger.Infof("Creating container from image %s...", image)

	cnt, err := c.createContainer(image)
	if err != nil {
		return nil, err
	}

	if err := c.attachContainer(ctx, cnt); err != nil {
		return nil, err
	}

	// It's required to not crash podman
	if c.config.Deployment.StartDelayMS > 0 {
		time.Sleep(time.Duration(c.config.Deployment.StartDelayMS) * time.Millisecond)
	}

	if err := c.startContainer(ctx, cnt); err != nil {
		return nil, err
	}

	c.logger.Infof("Container started.")

	return cnt, nil
}

func (c *connector) startContainer(ctx context.Context, cnt *connectorContainer) error {
	c.logger.Debugf("Starting container %s...", cnt.id)
	if err := c.cli.ContainerStart(ctx, cnt.id, container.StartOptions{}); err != nil {
		if err := cnt.Close(); err != nil {
			c.logger.Warningf("failed to remove previously-created container %s (%v)", cnt.id, err)
		}
		return fmt.Errorf("failed to start container %s (%w)", cnt.id, err)
	}
	return nil
}

func (c connector) attachContainer(ctx context.Context, cnt *connectorContainer) error {
	c.logger.Debugf("Attaching to container %s...", cnt.id)
	hijackedResponse, err := c.cli.ContainerAttach(
		ctx,
		cnt.id,
		container.AttachOptions{
			Stream: true,
			Stdin:  true,
			Stdout: true,
			Stderr: true,
			Logs:   true,
		},
	)
	if err != nil {
		if err2 := cnt.Close(); err2 != nil {
			c.logger.Warningf("failed to remove previously-created container %s (%v)", cnt.id, err2)
		}
		return fmt.Errorf("failed to attach to container %s (%w)", cnt.id, err)
	}
	cnt.hijackedResponse = &hijackedResponse
	cnt.multiplexedReader = multiplexedReader{
		reader: cnt.hijackedResponse.Reader,
	}
	return nil
}

func (c connector) createContainer(image string) (*connectorContainer, error) {
	containerConfig := c.config.Deployment.ContainerConfig
	if containerConfig == nil {
		containerConfig = &container.Config{}
	}
	containerConfig.Image = image
	containerConfig.Tty = false
	containerConfig.AttachStdin = true
	containerConfig.AttachStdout = true
	containerConfig.AttachStderr = true
	containerConfig.StdinOnce = false
	containerConfig.OpenStdin = true
	containerConfig.Cmd = []string{"--atp"}
	// Make sure Python is in unbuffered mode to avoid the output getting stuck.
	containerConfig.Env = append(containerConfig.Env, "PYTHON_UNBUFFERED=1")

	cont, err := c.cli.ContainerCreate(context.TODO(),
		containerConfig,
		c.config.Deployment.HostConfig,
		c.config.Deployment.NetworkConfig,
		c.config.Deployment.Platform,
		"",
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create container from image %s (%w)", image, err)
	}

	cnt := &connectorContainer{
		id:  cont.ID,
		cli: c.cli,
	}
	return cnt, nil
}

var validTagRegexp = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$`)

func (c connector) pullImage(ctx context.Context, image string) error {
	if c.config.Deployment.ImagePullPolicy == ImagePullPolicyNever {
		return nil
	}
	if c.config.Deployment.ImagePullPolicy == ImagePullPolicyIfNotPresent {
		var imageExists bool
		if _, err := c.cli.ImageInspect(ctx, image); err == nil {
			imageExists = true
		} else {
			imageExists = false
		}
		parts := strings.Split(image, ":")
		tag := parts[len(parts)-1]
		// It appears that the tag check is to defer invalid tag errors to docker
		if len(parts) > 1 && validTagRegexp.MatchString(tag) && imageExists {
			return nil
		}
	}
	c.logger.Debugf("Pulling image image %s...", image)
	pullReader, err := c.cli.ImagePull(ctx, image, img.PullOptions{})
	if err != nil {
		return fmt.Errorf("failed to pull image %s (%w)", image, err)
	}
	writer := &logWriter{
		logger: c.logger,
		buffer: []byte{},
		lock:   &sync.Mutex{},
	}
	if _, err := io.Copy(writer, pullReader); err != nil {
		return fmt.Errorf("failed to pull image %s (%w)", image, err)
	}
	_ = writer.Close()
	return nil
}

type logWriter struct {
	logger log.Logger
	buffer []byte
	lock   *sync.Mutex
}

func (l *logWriter) Write(p []byte) (n int, err error) {
	l.lock.Lock()
	defer l.lock.Unlock()
	l.buffer = append(l.buffer, p...)
	parts := strings.Split(string(l.buffer), "\n")
	for i := 0; i < len(parts)-2; i++ {
		line := map[string]any{}
		if err := json.Unmarshal([]byte(parts[i]), &line); err != nil {
			l.logger.Debugf("%s", parts[i])
		} else {
			if progress, ok := line["progress"]; ok {
				l.logger.Debugf("%s %s: %s", line["status"], line["id"], progress)
			} else if id, ok := line["id"]; ok {
				l.logger.Debugf("%s: %s", line["status"], id)
			} else {
				l.logger.Debugf("%s", line["status"])
			}
		}

	}
	l.buffer = []byte(parts[len(parts)-1])
	return len(p), nil
}

func (l *logWriter) Close() error {
	l.lock.Lock()
	defer l.lock.Unlock()
	if len(l.buffer) > 0 {
		l.logger.Debugf("%s", l.buffer)
		l.buffer = nil
	}
	return nil
}
