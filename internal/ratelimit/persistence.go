package ratelimit

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type persistedWindow struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	Used  int       `json:"used"`
}

type persistedState struct {
	Version  int                               `json:"version"`
	Quotas   map[string]persistedQuota         `json:"quotas"`
	Terminals map[string]map[string]persistedWindow `json:"terminals"`
}

type persistedQuota struct {
	Limit         int `json:"limit"`
	WindowSeconds int `json:"window_seconds"`
}

// load reads the durable quota table. A missing file is a first start: the
// default quota configuration is written immediately so operators can inspect
// it. Active windows survive the restart; expired windows are pruned and the
// pruned table is written back before serving starts.
func (l *Limiter) load() error {
	raw, err := os.ReadFile(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return l.persistLocked()
	}
	if err != nil {
		return fmt.Errorf("read quota state: %w", err)
	}

	var stored persistedState
	if err := json.Unmarshal(raw, &stored); err != nil {
		return fmt.Errorf("decode quota state: %w", err)
	}
	if stored.Version != 1 {
		return fmt.Errorf("unsupported quota state version %d", stored.Version)
	}
	if stored.Quotas == nil {
		return fmt.Errorf("quota state is missing its quota table")
	}
	if stored.Terminals == nil {
		stored.Terminals = map[string]map[string]persistedWindow{}
	}

	quotas := make(map[Category]Quota, len(Categories()))
	for _, category := range Categories() {
		encoded, found := stored.Quotas[string(category)]
		if !found {
			return fmt.Errorf("quota state is missing category %q", category)
		}
		quota := Quota{Limit: encoded.Limit, WindowSeconds: encoded.WindowSeconds}
		if !quota.Valid() {
			return fmt.Errorf("quota state has an invalid quota for category %q", category)
		}
		quotas[category] = quota
	}
	for name := range stored.Quotas {
		if !KnownCategory(Category(name)) {
			return fmt.Errorf("quota state contains unknown category %q", name)
		}
	}

	now := l.clock.Now()
	windows := make(map[string]map[Category]windowState, len(stored.Terminals))
	pruned := false
	for terminal, categories := range stored.Terminals {
		if err := ValidateTerminal(terminal); err != nil {
			return fmt.Errorf("quota state contains an invalid terminal id: %w", err)
		}
		if categories == nil {
			pruned = true
			continue
		}
		restored := map[Category]windowState{}
		for name, encoded := range categories {
			category := Category(name)
			if !KnownCategory(category) {
				return fmt.Errorf("quota state for terminal %q contains unknown category %q", terminal, name)
			}
			if encoded.Used < 0 {
				return fmt.Errorf("quota state for terminal %q category %q has a negative counter",
					terminal, name)
			}
			if encoded.End.IsZero() || encoded.Start.IsZero() || !encoded.End.After(encoded.Start) {
				return fmt.Errorf("quota state for terminal %q category %q has an invalid window",
					terminal, name)
			}
			// Windows already over at startup cannot explain any rejection.
			if !now.Before(encoded.End) {
				pruned = true
				continue
			}
			if encoded.Used == 0 {
				pruned = true
				continue
			}
			restored[category] = windowState{
				start: encoded.Start.UTC(),
				end:   encoded.End.UTC(),
				used:  encoded.Used,
			}
		}
		if len(restored) > 0 {
			windows[terminal] = restored
		} else {
			pruned = true
		}
	}

	l.quotas = quotas
	l.windows = windows
	if pruned {
		return l.persistLocked()
	}
	return nil
}

// persistLocked writes the complete table atomically. Callers must hold l.mu.
func (l *Limiter) persistLocked() error {
	stored := persistedState{
		Version:   1,
		Quotas:    make(map[string]persistedQuota, len(l.quotas)),
		Terminals: make(map[string]map[string]persistedWindow, len(l.windows)),
	}
	for category, quota := range l.quotas {
		stored.Quotas[string(category)] = persistedQuota{
			Limit:         quota.Limit,
			WindowSeconds: quota.WindowSeconds,
		}
	}
	for terminal, categories := range l.windows {
		encoded := make(map[string]persistedWindow, len(categories))
		for category, window := range categories {
			if window.used == 0 {
				continue
			}
			encoded[string(category)] = persistedWindow{
				Start: window.start.UTC(),
				End:   window.end.UTC(),
				Used:  window.used,
			}
		}
		if len(encoded) > 0 {
			stored.Terminals[terminal] = encoded
		}
	}
	return writeAtomically(l.path, stored)
}

func writeAtomically(path string, value any) (err error) {
	directory := filepath.Dir(path)
	handle, err := os.CreateTemp(directory, ".rate-limits-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary quota state: %w", err)
	}
	tempName := handle.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tempName)
		}
	}()

	encoder := json.NewEncoder(handle)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		_ = handle.Close()
		return fmt.Errorf("encode quota state: %w", err)
	}
	if err := handle.Sync(); err != nil {
		_ = handle.Close()
		return fmt.Errorf("commit quota state bytes: %w", err)
	}
	if err := handle.Close(); err != nil {
		return fmt.Errorf("close temporary quota state: %w", err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("replace quota state: %w", err)
	}
	removeTemp = false
	return nil
}
