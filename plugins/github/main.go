// Command github is Gator's reference plugin for GitHub: issues become tasks, a finished
// worker job opens a pull request with its report, CI check runs become gate checks, and
// phase labels follow the task. It talks to the REST API with a fine-grained token.
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/onegator/gator/internal/server/netguard"
	"github.com/onegator/gator/plugin"
)

func main() {
	// The API address is a setting; its requests stay off private addresses unless the server
	// allows them (GATOR_OUTBOUND_ALLOW_PRIVATE, passed on by the host).
	g := &gh{http: netguard.FromEnv(os.Getenv).Client(20 * time.Second)}
	if err := plugin.Serve(g.handlers()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
