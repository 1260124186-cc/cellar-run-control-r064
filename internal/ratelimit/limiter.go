package ratelimit

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/clock"
)

const limitsFileName = "rate_limits.json"

// DeniedError is a rate-limit verdict. It is never a business error and is
// produced before any business state is touched.
type DeniedError struct {
	Decision Decision
}

func (e *DeniedError) Error() string {
	seconds := ceilSeconds(e.Decision.RetryAfter)
	return fmt.Sprintf("terminal %q exceeded the %s quota; retry after %d second(s)",
		e.Decision.Terminal, e.Decision.Category, seconds)
}

// RetryAfterSeconds is the integer-second value suitable for Retry-After.
func (e *DeniedError) RetryAfterSeconds() int {
	return ceilSeconds(e.Decision.RetryAfter)
}

type windowState struct {
	start time.Time
	end   time.Time
	used  int
}

// Limiter is a concurrency-safe, per-terminal fixed-window quota table.
type Limiter struct {
	mu      sync.Mutex
	path    string
	clock   clock.Clock
	quotas  map[Category]Quota
	windows map[string]map[Category]windowState

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// NewFor opens (or initializes) the local quota table for dataDir. Existing
// windows that are still active at process start are restored; expired ones are
// pruned and the trimmed table is persisted immediately. A corrupt or
// inconsistent limits file stops startup rather than silently lifting limits.
func NewFor(dataDir string, timeSource clock.Clock) (*Limiter, error) {
	if timeSource == nil {
		timeSource = clock.System{}
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	limiter := &Limiter{
		path:    filepath.Join(dataDir, limitsFileName),
		clock:   timeSource,
		quotas:  DefaultQuotas(),
		windows: map[string]map[Category]windowState{},
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	if err := limiter.load(); err != nil {
		return nil, err
	}
	go limiter.sweepLoop()
	return limiter, nil
}

// Allow consumes one slot for terminal in category and returns the resulting
// verdict. The check, the counter update, and persistence happen atomically:
// a burst of concurrent requests is serialized so each receives one definite
// answer. A denied request performs no persistence write and can never reach
// business state.
func (l *Limiter) Allow(terminal string, category Category) (Decision, error) {
	if err := ValidateTerminal(terminal); err != nil {
		return Decision{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.clock.Now()
	quota, found := l.quotas[category]
	if !found {
		return Decision{}, fmt.Errorf("unknown operation category %q", category)
	}

	categories := l.windows[terminal]
	if categories == nil {
		categories = map[Category]windowState{}
		l.windows[terminal] = categories
	}
	window, exists := categories[category]
	if !exists || !now.Before(window.end) {
		// First call in this terminal/category, or the previous window has
		// elapsed. Window boundaries are anchored at the first accepted
		// request, never at wall-clock multiples.
		window = windowState{
			start: now,
			end:   now.Add(quota.window()),
			used:  0,
		}
	}

	decision := Decision{
		Terminal:    terminal,
		Category:    category,
		Limit:       quota.Limit,
		WindowStart: window.start.UTC(),
		WindowEnd:   window.end.UTC(),
	}

	if window.used >= quota.Limit {
		decision.Used = window.used
		decision.Remaining = 0
		decision.RetryAfter = window.end.Sub(now)
		if decision.RetryAfter < 0 {
			decision.RetryAfter = 0
		}
		return decision, &DeniedError{Decision: decision}
	}

	window.used++
	categories[category] = window

	// Persist before releasing the gate. On failure, roll the counter back and
	// fail closed so a restart cannot erase an enforced window.
	if err := l.persistLocked(); err != nil {
		window.used--
		if window.used == 0 {
			delete(categories, category)
		} else {
			categories[category] = window
		}
		return Decision{}, fmt.Errorf("persist quota state: %w", err)
	}

	decision.Allowed = true
	decision.Used = window.used
	decision.Remaining = quota.Limit - window.used
	return decision, nil
}

// Quotas returns a copy of the currently enforced quota table, ordered by the
// canonical category list.
func (l *Limiter) Quotas() map[Category]Quota {
	l.mu.Lock()
	defer l.mu.Unlock()
	result := make(map[Category]Quota, len(l.quotas))
	for category, quota := range l.quotas {
		result[category] = quota
	}
	return result
}

// UpdateQuota replaces one category quota at runtime. A window already running
// for a terminal keeps the limit it was opened with (the request sees one
// stable answer for that window); every window opened afterwards uses the new
// quota. The new configuration is durably persisted before the call returns.
func (l *Limiter) UpdateQuota(category Category, quota Quota) error {
	if !KnownCategory(category) {
		return fmt.Errorf("unknown operation category %q", category)
	}
	if !quota.Valid() {
		return fmt.Errorf("quota must allow at least one request and a window of %d..%d seconds",
			minWindowSeconds, maxWindowSeconds)
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	previous := l.quotas[category]
	l.quotas[category] = quota
	if err := l.persistLocked(); err != nil {
		l.quotas[category] = previous
		return fmt.Errorf("persist quota configuration: %w", err)
	}
	return nil
}

// Close stops background maintenance. It does not erase persisted state.
func (l *Limiter) Close() {
	l.stopOnce.Do(func() {
		close(l.stop)
		<-l.done
	})
}

// sweepLoop periodically drops expired windows so idle terminals do not keep
// the in-memory table growing. Persistence happens only when something was
// actually removed; a concurrent Allow always serializes against the same lock.
func (l *Limiter) sweepLoop() {
	defer close(l.done)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			l.sweep()
		}
	}
}

func (l *Limiter) sweep() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock.Now()
	changed := false
	for terminal, categories := range l.windows {
		for category, window := range categories {
			if !now.Before(window.end) {
				delete(categories, category)
				changed = true
			}
		}
		if len(categories) == 0 {
			delete(l.windows, terminal)
		}
	}
	if changed {
		_ = l.persistLocked()
	}
}

func ceilSeconds(duration time.Duration) int {
	seconds := duration / time.Second
	if duration%time.Second > 0 {
		seconds++
	}
	if seconds < 1 {
		return 1
	}
	return int(seconds)
}
