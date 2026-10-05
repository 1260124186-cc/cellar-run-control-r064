// Package ratelimit enforces local write quotas keyed by cellar terminal and
// operation category. The service runs on a single workshop machine: when a
// tablet is refreshed repeatedly or a script hammers an endpoint, exhausting a
// quota rejects further writes quickly with an explicit wait instead of
// queueing them against the serialized business store.
//
// Every decision is made under one mutex, so bursts, window rollover, and
// runtime configuration changes each observe exactly one consistent result.
// Only quota state transitions touch durable storage; accepted requests and
// requests served while blocked never perform disk I/O. Blocked pairs are
// persisted so that a controller restart still enforces penalties whose window
// has not elapsed.
package ratelimit

import (
	"fmt"
	"sync"
	"time"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/clock"
)

// Category identifies the operation class a write request belongs to.
type Category string

const (
	// CategoryFormula covers formula creation, step updates, approval, and
	// retirement.
	CategoryFormula Category = "formula"
	// CategoryVessel covers vessel registration, cleaning, and retirement.
	CategoryVessel Category = "vessel"
	// CategoryRun covers run lifecycle writes other than observations.
	CategoryRun Category = "run"
	// CategoryObservation covers appended gravity observations.
	CategoryObservation Category = "observation"
)

// Categories returns every enforced operation category.
func Categories() []Category {
	return []Category{CategoryFormula, CategoryVessel, CategoryRun, CategoryObservation}
}

// CategoryRule is the per-category quota: at most Limit accepted requests in
// any trailing Window.
type CategoryRule struct {
	Limit  int
	Window time.Duration
}

// Config is the complete quota configuration.
type Config struct {
	// Rules must contain every Category.
	Rules map[Category]CategoryRule
	// Penalty is how long a terminal stays blocked once it exceeds a quota.
	// It must be at least as long as the longest category window so a block
	// never expires before the burst that triggered it leaves its window.
	Penalty time.Duration
}

// DefaultConfig returns permissive but finite local defaults.
func DefaultConfig() Config {
	return Config{
		Rules: map[Category]CategoryRule{
			CategoryFormula:     {Limit: 30, Window: time.Minute},
			CategoryVessel:      {Limit: 30, Window: time.Minute},
			CategoryRun:         {Limit: 30, Window: time.Minute},
			CategoryObservation: {Limit: 60, Window: time.Minute},
		},
		Penalty: 2 * time.Minute,
	}
}

// Validated returns a normalized copy or an error describing the invalid rule.
func (c Config) Validated() (Config, error) {
	validated := Config{Rules: make(map[Category]CategoryRule, len(Categories()))}
	longestWindow := time.Duration(0)
	for _, category := range Categories() {
		rule, ok := c.Rules[category]
		if !ok {
			return Config{}, fmt.Errorf("quota rule for %q is missing", category)
		}
		if rule.Limit < 1 {
			return Config{}, fmt.Errorf("quota limit for %q must be at least 1", category)
		}
		if rule.Window < time.Second {
			return Config{}, fmt.Errorf("quota window for %q must be at least 1 second", category)
		}
		validated.Rules[category] = CategoryRule{Limit: rule.Limit, Window: rule.Window}
		if rule.Window > longestWindow {
			longestWindow = rule.Window
		}
	}
	if c.Penalty < longestWindow {
		return Config{}, fmt.Errorf("quota penalty %s must be at least the longest window %s",
			c.Penalty, longestWindow)
	}
	validated.Penalty = c.Penalty
	return validated, nil
}

// Decision is the single authoritative outcome of one quota evaluation.
type Decision struct {
	Allowed    bool
	Terminal   string
	Category   Category
	Limit      int
	Remaining  int
	RetryAfter time.Duration
	BlockedAt  time.Time
	BlockUntil time.Time
	// Restored reports whether the active block was recovered from disk after
	// a controller restart.
	Restored bool
}

// maxTrackedTerminals bounds in-memory state if clients spray unknown terminal
// identifiers. Excess unknown terminals share an overflow bucket instead of
// growing the map without limit.
const maxTrackedTerminals = 10000

const overflowTerminal = "overflow:unidentified"

// Limiter tracks per-terminal, per-category request history and active blocks.
type Limiter struct {
	mu        sync.Mutex
	path      string
	clk       clock.Clock
	cfg       Config
	terminals map[string]*terminalState
}

type terminalState struct {
	buckets map[Category]*bucket
	blocks  map[Category]block
}

type bucket struct {
	stamps []time.Time
}

type block struct {
	since    time.Time
	until    time.Time
	restored bool
}

// Open validates the configuration, loads any persisted blocks, and returns a
// ready limiter. When path is empty the limiter keeps blocks in memory only.
func Open(path string, cfg Config, timeSource clock.Clock) (*Limiter, error) {
	if timeSource == nil {
		timeSource = clock.System{}
	}
	validated, err := cfg.Validated()
	if err != nil {
		return nil, err
	}
	limiter := &Limiter{
		path:      path,
		clk:       timeSource,
		cfg:       validated,
		terminals: map[string]*terminalState{},
	}
	if err := limiter.load(); err != nil {
		return nil, err
	}
	return limiter, nil
}

// Allow records one write attempt and returns its authoritative decision. A
// rejected request never needs to touch any business state.
func (l *Limiter) Allow(terminal string, category Category) (Decision, error) {
	now := l.clk.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	rule, ok := l.cfg.Rules[category]
	if !ok {
		return Decision{}, fmt.Errorf("unknown quota category %q", category)
	}
	state := l.obtainTerminal(terminal)

	if active, exists := state.blocks[category]; exists {
		if active.until.After(now) {
			return blockedDecision(terminal, category, rule, now, active), nil
		}
		// Penalty elapsed. Drop it in memory and rewrite the durable record
		// once so expired entries cannot accumulate forever.
		delete(state.blocks, category)
		l.forgetTerminalIfEmpty(terminal, state)
		// Expiry cleanup is best effort: a failed rewrite only means the stale
		// record is reconciled at the next block transition or startup prune.
		_ = l.persistLocked(now)
	}

	history := prune(state.buckets[category], rule.Window, now)
	if len(history) >= rule.Limit {
		penalty := block{since: now, until: now.Add(l.cfg.Penalty)}
		state.blocks[category] = penalty
		if err := l.persistLocked(now); err != nil {
			// Keep the in-memory block either way, but surface the fault so
			// the caller answers honestly with a 5xx instead of claiming a
			// durable block was written.
			return Decision{}, err
		}
		return blockedDecision(terminal, category, rule, now, penalty), nil
	}

	history = append(history, now)
	if state.buckets == nil {
		state.buckets = map[Category]*bucket{}
	}
	state.buckets[category] = &bucket{stamps: history}
	return Decision{
		Allowed:   true,
		Terminal:  terminal,
		Category:  category,
		Limit:     rule.Limit,
		Remaining: rule.Limit - len(history),
	}, nil
}

// UpdateConfig swaps the quota configuration atomically. In-flight evaluations
// observe either the old rules or the new rules, never a mixture. Existing
// request history is pruned against the new windows and active blocks keep
// their original expiry, so lowering a limit never silently erases a penalty.
func (l *Limiter) UpdateConfig(cfg Config) error {
	validated, err := cfg.Validated()
	if err != nil {
		return err
	}
	now := l.clk.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, state := range l.terminals {
		for category, value := range state.buckets {
			if rule, ok := validated.Rules[category]; ok {
				value.stamps = prune(value, rule.Window, now)
			}
		}
	}
	l.cfg = validated
	return nil
}

// CategoryStatus explains the current quota posture for one category.
type CategoryStatus struct {
	Limit             int        `json:"limit"`
	WindowSeconds     int        `json:"window_seconds"`
	Remaining         int        `json:"remaining"`
	Blocked           bool       `json:"blocked"`
	Restored          bool       `json:"restored,omitempty"`
	BlockedAt         *time.Time `json:"blocked_at,omitempty"`
	BlockUntil        *time.Time `json:"blocked_until,omitempty"`
	RetryAfterSeconds int        `json:"retry_after_seconds,omitempty"`
}

// TerminalStatus is the read-only explanation surfaced at /quota-status.
type TerminalStatus struct {
	Terminal    string                    `json:"terminal"`
	PenaltySecs int                       `json:"penalty_seconds"`
	Categories  map[string]CategoryStatus `json:"categories"`
}

// Status reports quota state without recording anything: a read-only observer
// must not create buckets or move counters.
func (l *Limiter) Status(terminal string) TerminalStatus {
	now := l.clk.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	state := l.terminals[terminal]
	result := TerminalStatus{
		Terminal:    terminal,
		PenaltySecs: int(l.cfg.Penalty / time.Second),
		Categories:  map[string]CategoryStatus{},
	}
	for _, category := range Categories() {
		rule := l.cfg.Rules[category]
		status := CategoryStatus{
			Limit:         rule.Limit,
			WindowSeconds: int(rule.Window / time.Second),
			Remaining:     rule.Limit,
		}
		if state != nil {
			if active, exists := state.blocks[category]; exists && active.until.After(now) {
				status.Blocked = true
				status.Restored = active.restored
				since := active.since
				until := active.until
				status.BlockedAt = &since
				status.BlockUntil = &until
				status.RetryAfterSeconds = ceilSeconds(now, active.until)
				status.Remaining = 0
			} else {
				used := len(prune(state.buckets[category], rule.Window, now))
				if used <= rule.Limit {
					status.Remaining = rule.Limit - used
				}
			}
		}
		result.Categories[string(category)] = status
	}
	return result
}

func (l *Limiter) obtainTerminal(terminal string) *terminalState {
	if state, ok := l.terminals[terminal]; ok {
		return state
	}
	if len(l.terminals) >= maxTrackedTerminals {
		if state, ok := l.terminals[overflowTerminal]; ok {
			return state
		}
		terminal = overflowTerminal
	}
	state := &terminalState{
		buckets: map[Category]*bucket{},
		blocks:  map[Category]block{},
	}
	l.terminals[terminal] = state
	return state
}

func (l *Limiter) forgetTerminalIfEmpty(terminal string, state *terminalState) {
	if terminal == overflowTerminal {
		return
	}
	if len(state.buckets) == 0 && len(state.blocks) == 0 {
		delete(l.terminals, terminal)
	}
}

// prune returns the request stamps still inside the trailing window. It never
// mutates the input slice, so read-only callers cannot disturb history that an
// in-flight Allow evaluation is about to retain.
func prune(value *bucket, window time.Duration, now time.Time) []time.Time {
	if value == nil {
		return nil
	}
	cutoff := now.Add(-window)
	kept := make([]time.Time, 0, len(value.stamps))
	for _, stamp := range value.stamps {
		if stamp.After(cutoff) {
			kept = append(kept, stamp)
		}
	}
	return kept
}

func blockedDecision(
	terminal string,
	category Category,
	rule CategoryRule,
	now time.Time,
	penalty block,
) Decision {
	return Decision{
		Allowed:    false,
		Terminal:   terminal,
		Category:   category,
		Limit:      rule.Limit,
		Remaining:  0,
		RetryAfter: penalty.until.Sub(now),
		BlockedAt:  penalty.since,
		BlockUntil: penalty.until,
		Restored:   penalty.restored,
	}
}

func ceilSeconds(now, until time.Time) int {
	remaining := until.Sub(now)
	if remaining <= 0 {
		return 0
	}
	return int((remaining + time.Second - 1) / time.Second)
}
