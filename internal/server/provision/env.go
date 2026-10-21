package provision

import (
	"fmt"
	"strings"

	"github.com/onegator/gator/internal/server/runners"
)

// Settings is what the server needs to start cloud runners, read from its environment.
type Settings struct {
	Provisioner runners.Provisioner // nil when cloud runners are not started on demand
	ServerURL   string              // where a machine reaches the server
	Env         map[string]string   // passed to every machine: the backend's credentials
}

// FromEnv reads GATOR_CLOUD_*:
//
//	GATOR_CLOUD_PROVISIONER   docker | fly; empty starts nothing
//	GATOR_CLOUD_RUNNER_IMAGE  the image built from Dockerfile.runner
//	GATOR_CLOUD_SERVER_URL    how a machine reaches this server
//	GATOR_CLOUD_RUNNER_ENV    names of server variables every machine gets, e.g. CLAUDE_CODE_OAUTH_TOKEN
//	GATOR_CLOUD_DOCKER_NETWORK, GATOR_CLOUD_DOCKER_ARGS   for docker
//	FLY_API_TOKEN, FLY_APP, GATOR_CLOUD_FLY_REGION        for fly
//
// Only the variables named in GATOR_CLOUD_RUNNER_ENV leave the server, and a name it lists
// that is not set is an error: a machine without its login would fail every job it takes.
func FromEnv(getenv func(string) string) (Settings, error) {
	kind := strings.TrimSpace(getenv("GATOR_CLOUD_PROVISIONER"))
	if kind == "" {
		return Settings{}, nil
	}
	s := Settings{ServerURL: strings.TrimSpace(getenv("GATOR_CLOUD_SERVER_URL")), Env: map[string]string{}}
	if s.ServerURL == "" {
		return Settings{}, fmt.Errorf("GATOR_CLOUD_SERVER_URL is required with GATOR_CLOUD_PROVISIONER")
	}
	image := strings.TrimSpace(getenv("GATOR_CLOUD_RUNNER_IMAGE"))
	if image == "" {
		return Settings{}, fmt.Errorf("GATOR_CLOUD_RUNNER_IMAGE is required with GATOR_CLOUD_PROVISIONER")
	}
	for name := range strings.SplitSeq(getenv("GATOR_CLOUD_RUNNER_ENV"), ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if strings.HasPrefix(name, "GATOR_") {
			return Settings{}, fmt.Errorf("GATOR_CLOUD_RUNNER_ENV: %s is the server's own; machines get only what their runner needs", name)
		}
		v := getenv(name)
		if v == "" {
			return Settings{}, fmt.Errorf("GATOR_CLOUD_RUNNER_ENV names %s, which is not set", name)
		}
		s.Env[name] = v
	}
	switch kind {
	case "docker":
		s.Provisioner = &Docker{Image: image, Network: getenv("GATOR_CLOUD_DOCKER_NETWORK"),
			Args: strings.Fields(getenv("GATOR_CLOUD_DOCKER_ARGS"))}
	case "fly":
		f := &Fly{Token: getenv("FLY_API_TOKEN"), App: getenv("FLY_APP"), Image: image, Region: getenv("GATOR_CLOUD_FLY_REGION")}
		if f.Token == "" || f.App == "" {
			return Settings{}, fmt.Errorf("the fly provisioner needs FLY_API_TOKEN and FLY_APP")
		}
		s.Provisioner = f
	default:
		return Settings{}, fmt.Errorf("GATOR_CLOUD_PROVISIONER must be docker or fly, not %q", kind)
	}
	return s, nil
}
