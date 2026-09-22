// Command honeybadger is Gator's monitoring plugin for Honeybadger: a fault becomes an
// incident, resolving it there closes it here, and finishing the incident task resolves it
// there.
package main

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/onegator/gator/plugin"
)

func main() {
	hb := &honeybadger{http: &http.Client{Timeout: 20 * time.Second}}
	if err := plugin.Serve(hb.handlers()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
