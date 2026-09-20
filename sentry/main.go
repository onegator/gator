// Command sentry is Gator's monitoring plugin for Sentry: issue alerts become incidents,
// resolving one in Sentry closes it here, and finishing the incident task resolves it there.
package main

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/onegator/gator/plugin"
)

func main() {
	s := &sentry{http: &http.Client{Timeout: 20 * time.Second}}
	if err := plugin.Serve(s.handlers()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
