package e2e

import (
	"context"
	"fmt"
	"time"

	"github.com/onegator/gator/internal/server/auth"
)

func runnerToken(h *harness) (string, error) {
	return auth.IssueRunnerToken(context.Background(), h.pool, fmt.Sprintf("e2e-runner-b-%d", time.Now().UnixNano()))
}
