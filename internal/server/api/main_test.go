package api

import (
	"testing"

	"github.com/onegator/gator/internal/server/store/storetest"
)

// TestMain gives the package a database of its own: its harness runs the relay and the
// conversations' consumer, which act on every event of the database they follow.
func TestMain(m *testing.M) { storetest.Main(m, "api") }
