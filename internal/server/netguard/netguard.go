// Package netguard is the server's name for plugin/netguard, the outbound policy the server and
// plugins share: no private, loopback, link-local or metadata address, no plain http, unless the
// operator allows them (GATOR_OUTBOUND_ALLOW_PRIVATE=1). The policy lives in the public SDK so that
// plugins outside this repository can hold their requests to it too.
package netguard

import "github.com/onegator/gator/plugin/netguard"

// Policy is what the server may reach.
type Policy = netguard.Policy

// ErrRefused is an address the policy keeps the server from reaching.
var ErrRefused = netguard.ErrRefused

// FromEnv reads GATOR_OUTBOUND_ALLOW_PRIVATE.
func FromEnv(getenv func(string) string) Policy { return netguard.FromEnv(getenv) }
