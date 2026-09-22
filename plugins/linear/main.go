// Command linear is Gator's tracker plugin for Linear: an issue becomes a task, the task's
// phase is mirrored in the issue's workflow state, and the task detail links back to it.
package main

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/onegator/gator/plugin"
)

func main() {
	l := &linear{http: &http.Client{Timeout: 20 * time.Second}}
	if err := plugin.Serve(l.handlers()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
