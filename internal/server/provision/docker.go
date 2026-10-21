// Package provision starts gator-runner machines for cloud jobs: Docker for a laptop or a
// single VPS, Fly Machines for capacity nobody has to keep running.
package provision

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/onegator/gator/internal/server/runners"
)

// Docker runs each cloud runner as a container through the docker CLI, so DOCKER_HOST and
// docker contexts work as they do for a person: a local engine in development, a VPS's engine
// over ssh:// in production.
type Docker struct {
	Bin     string // default "docker"
	Image   string // an image built from Dockerfile.runner
	Network string // optional --network, e.g. the compose network the server is on
	// Args go to `docker run` before the image: --add-host, --cpus, --memory and the like.
	Args []string
	// Env is the docker CLI's own environment. Nil takes the server's, which is where
	// DOCKER_HOST comes from; none of it reaches the container unless named in the spec.
	Env []string
}

var _ runners.Provisioner = (*Docker)(nil)

// Name implements runners.Provisioner.
func (d *Docker) Name() string { return "docker" }

func (d *Docker) bin() string {
	if d.Bin == "" {
		return "docker"
	}
	return d.Bin
}

// Start implements runners.Provisioner. Values travel in the CLI's environment and only their
// names on its command line (`-e NAME`), so the token and the backend's credentials never show
// up in a process list.
func (d *Docker) Start(ctx context.Context, spec runners.MachineSpec) (string, error) {
	if d.Image == "" {
		return "", fmt.Errorf("docker: no runner image configured")
	}
	args := []string{"run", "--detach", "--rm", "--name", spec.Name,
		"--label", "dev.onegator.gator.job=" + spec.JobID, "--label", "dev.onegator.gator.project=" + spec.ProjectID}
	if d.Network != "" {
		args = append(args, "--network", d.Network)
	}
	args = append(args, d.Args...)
	env := d.Env
	if env == nil {
		env = os.Environ()
	}
	env = slices.Clone(env)
	keys := make([]string, 0, len(spec.Env))
	for k := range spec.Env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		args = append(args, "--env", k)
		env = append(env, k+"="+spec.Env[k])
	}
	args = append(args, d.Image)
	out, err := d.run(ctx, env, args...)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(out)
	if id == "" {
		return spec.Name, nil
	}
	return id, nil
}

// Stop implements runners.Provisioner. The container was started with --rm, so after its
// runner exits there is nothing to stop, and docker saying so is success.
func (d *Docker) Stop(ctx context.Context, handle string) error {
	env := d.Env
	if env == nil {
		env = os.Environ()
	}
	_, err := d.run(ctx, env, "rm", "--force", handle)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "no such container") {
		return nil
	}
	return err
}

func (d *Docker) run(ctx context.Context, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, d.bin(), args...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 500 {
			msg = msg[:500]
		}
		return "", fmt.Errorf("docker %s: %v: %s", args[0], err, msg)
	}
	return stdout.String(), nil
}
