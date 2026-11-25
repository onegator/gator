// Command slack bridges a Slack workspace and a Gator project: messages in the channels the project
// connects become its conversations (threads stay threads), the project's agents answer there and
// in Slack, and a decision card can be approved from Slack by an account its person linked. It
// verifies every request against the app's signing secret and talks to Slack's Web API with the bot
// token Connect Slack stores, through netguard.
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/onegator/gator/plugin"
	"github.com/onegator/gator/plugin/netguard"
)

func main() {
	s := &slack{http: netguard.FromEnv(os.Getenv).Client(20 * time.Second), now: time.Now, names: map[string]string{}}
	if err := plugin.Serve(s.handlers()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
