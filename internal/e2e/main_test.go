package e2e

import (
	"testing"

	"github.com/onegator/gator/internal/server/store/storetest"
)

// TestMain gives the package a database of its own. Its servers follow the outbox like a real
// one does — agents answer mentions, cards follow work — and on the database every other package
// tests against, they would answer that package's mentions too, from under its tests.
func TestMain(m *testing.M) { storetest.Main(m, "e2e") }
