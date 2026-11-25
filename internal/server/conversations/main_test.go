package conversations

import (
	"testing"

	"github.com/onegator/gator/internal/server/store/storetest"
)

// TestMain gives the package a database of its own: its card tests run the outbox's consumer.
func TestMain(m *testing.M) { storetest.Main(m, "conversations") }
