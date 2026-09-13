package limits

import (
	"errors"
	"sync"
	"time"
)

// ErrOpen is returned while a breaker refuses calls.
var ErrOpen = errors.New("circuit open")

// State of a breaker.
type State int

const (
	Closed State = iota
	Open
	HalfOpen
)

func (s State) String() string {
	switch s {
	case Open:
		return "open"
	case HalfOpen:
		return "half-open"
	}
	return "closed"
}

// Breaker trips after `threshold` consecutive failures, refuses calls for `cooldown`,
// then lets one probe through. A success closes it; a failure re-opens it.
type Breaker struct {
	mu        sync.Mutex
	threshold int
	cooldown  time.Duration
	failures  int
	state     State
	openedAt  time.Time
	now       func() time.Time
	OnTrip    func() // optional hook, called outside the lock
}

// NewBreaker creates a closed breaker.
func NewBreaker(threshold int, cooldown time.Duration) *Breaker {
	return &Breaker{threshold: threshold, cooldown: cooldown, now: time.Now}
}

// State returns the current state, transitioning Open → HalfOpen after cooldown.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stateLocked()
}

func (b *Breaker) stateLocked() State {
	if b.state == Open && b.now().Sub(b.openedAt) >= b.cooldown {
		b.state = HalfOpen
	}
	return b.state
}

// Do runs fn unless the breaker is open. In half-open only one call proceeds at a time.
func (b *Breaker) Do(fn func() error) error {
	b.mu.Lock()
	switch b.stateLocked() {
	case Open:
		b.mu.Unlock()
		return ErrOpen
	case HalfOpen:
		// mark as open again so concurrent callers are refused until this probe ends
		b.state = Open
		b.openedAt = b.now()
	}
	b.mu.Unlock()

	err := fn()

	b.mu.Lock()
	tripped := false
	if err != nil {
		b.failures++
		if b.failures >= b.threshold {
			if b.state != Open {
				tripped = true
			}
			b.state = Open
			b.openedAt = b.now()
		}
	} else {
		b.failures = 0
		b.state = Closed
	}
	hook := b.OnTrip
	b.mu.Unlock()
	if tripped && hook != nil {
		hook()
	}
	return err
}

// Reset closes the breaker manually (an admin re-enabling a plugin).
func (b *Breaker) Reset() {
	b.mu.Lock()
	b.failures, b.state = 0, Closed
	b.mu.Unlock()
}
