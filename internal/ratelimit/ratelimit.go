// Package ratelimit enforces local, per-cellar-terminal request quotas for
// mutating operation categories. It is an in-process fixed-window counter that
// persists its own bookkeeping to a file separate from business state, so a
// controller restart keeps an explainable view of terminals that are still
// inside a limiting window.
package ratelimit

import (
	"fmt"
	"regexp"
	"time"
)

// Category groups mutating routes that share one quota per terminal.
type Category string

const (
	// CategoryFormula covers formula creation, step edits, approval, retirement.
	CategoryFormula Category = "formula"
	// CategoryVessel covers vessel registration, cleaning, retirement.
	CategoryVessel Category = "vessel"
	// CategoryRun covers run creation and lifecycle transitions.
	CategoryRun Category = "run"
	// CategoryObservation covers gravity observation writes only.
	CategoryObservation Category = "observation"
)

// CodeRateLimited is the stable error code returned on a rejected request.
const CodeRateLimited = "rate_limited"

const (
	minWindowSeconds = 1
	maxWindowSeconds = 3600
)

// Quota is the maximum number of accepted mutating requests one terminal may
// make within a fixed window.
type Quota struct {
	Limit         int `json:"limit"`
	WindowSeconds int `json:"window_seconds"`
}

func (q Quota) window() time.Duration {
	return time.Duration(q.WindowSeconds) * time.Second
}

// Valid reports whether the quota can be applied.
func (q Quota) Valid() bool {
	return q.Limit >= 1 && q.WindowSeconds >= minWindowSeconds && q.WindowSeconds <= maxWindowSeconds
}

// DefaultQuotas returns the local quota applied before any runtime adjustment.
// The defaults tolerate normal terminal use while stopping a refresh loop or a
// script after a few dozen calls.
func DefaultQuotas() map[Category]Quota {
	return map[Category]Quota{
		CategoryFormula:     {Limit: 60, WindowSeconds: 60},
		CategoryVessel:      {Limit: 60, WindowSeconds: 60},
		CategoryRun:         {Limit: 60, WindowSeconds: 60},
		CategoryObservation: {Limit: 30, WindowSeconds: 60},
	}
}

// Categories lists every enforced operation category.
func Categories() []Category {
	return []Category{CategoryFormula, CategoryVessel, CategoryRun, CategoryObservation}
}

// KnownCategory reports whether category is an enforced operation category.
func KnownCategory(category Category) bool {
	switch category {
	case CategoryFormula, CategoryVessel, CategoryRun, CategoryObservation:
		return true
	default:
		return false
	}
}

func validateQuotaSet(quotas map[Category]Quota) error {
	for _, category := range Categories() {
		quota, found := quotas[category]
		if !found {
			return fmt.Errorf("missing quota for category %q", category)
		}
		if !quota.Valid() {
			return fmt.Errorf("quota for category %q must allow at least one request and a window of %d..%d seconds",
				category, minWindowSeconds, maxWindowSeconds)
		}
	}
	for category := range quotas {
		if !KnownCategory(category) {
			return fmt.Errorf("unknown operation category %q", category)
		}
	}
	return nil
}

var terminalPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

// ValidateTerminal checks an explicit X-Cellar-Terminal-ID value.
func ValidateTerminal(terminal string) error {
	if !terminalPattern.MatchString(terminal) {
		return fmt.Errorf("terminal id must be 1..64 characters using letters, digits, dot, colon, underscore or hyphen")
	}
	return nil
}

// Decision is the single verdict produced for a quota check. Every request
// receives exactly one verdict, including bursts and requests that straddle a
// window boundary.
type Decision struct {
	Allowed     bool
	Terminal    string
	Category    Category
	Limit       int
	Used        int
	Remaining   int
	WindowStart time.Time
	WindowEnd   time.Time
	// RetryAfter is zero for an allowed request and positive for a denied one.
	RetryAfter time.Duration
}
