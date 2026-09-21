// Command github is Gator's reference plugin for GitHub: issues become tasks, a finished
// worker job opens a pull request with its report, CI check runs become gate checks, and
// phase labels follow the task. It talks to the REST API with a fine-grained token.
package main

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/onegator/gator/plugin"
)

func main() {
	g := &gh{http: &http.Client{Timeout: 20 * time.Second}}
	if err := plugin.Serve(g.handlers()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
