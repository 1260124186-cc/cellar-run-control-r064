package ratelimit

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// blocksFileName holds only currently active blocks. Accepted request history
// is deliberately not persisted: after a restart the sliding window starts
// fresh, but a penalty whose window is still active keeps being enforced and is
// marked as restored so clients get an explainable answer.
const blocksFileName = "quota-blocks.json"

const blocksSchemaVersion = 1

type persistedBlock struct {
	Since    time.Time `json:"since"`
	Until    time.Time `json:"until"`
	Restored bool      `json:"restored"`
}

type blocksFile struct {
	SchemaVersion int                       `json:"schema_version"`
	UpdatedAt     time.Time                 `json:"updated_at"`
	Blocks        map[string]persistedBlock `json:"blocks"`
}

// terminalBlock flattens the nested terminal/category index for JSON output.
type terminalBlock struct {
	terminal string
	category Category
	block    block
}

// load reads the durable blocks file, rejecting a corrupt or mismatched file at
// startup and dropping blocks whose penalty has already elapsed.
func (l *Limiter) load() error {
	if l.path == "" {
		return nil
	}
	raw, err := os.ReadFile(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read quota blocks: %w", err)
	}
	var loaded blocksFile
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return fmt.Errorf("decode quota blocks: %w", err)
	}
	if loaded.SchemaVersion != blocksSchemaVersion {
		return fmt.Errorf("quota blocks schema version %d is not supported", loaded.SchemaVersion)
	}
	now := l.clk.Now()
	for key, value := range loaded.Blocks {
		terminal, category, valid := parseBlockKey(key)
		if !valid {
			return fmt.Errorf("quota blocks contain invalid key %q", key)
		}
		if _, ok := l.cfg.Rules[category]; !ok {
			return fmt.Errorf("quota blocks contain unknown category %q", category)
		}
		if value.Until.IsZero() || value.Until.Before(value.Since) {
			return fmt.Errorf("quota blocks contain an invalid penalty window for %q", key)
		}
		if !value.Until.After(now) {
			continue
		}
		state := l.obtainTerminal(terminal)
		state.blocks[category] = block{
			since:    value.Since,
			until:    value.Until,
			restored: true,
		}
	}
	return nil
}

// persistLocked rewrites the blocks file atomically while the caller holds
// l.mu. Only active blocks are written, so the file cannot grow without bound.
func (l *Limiter) persistLocked(now time.Time) error {
	if l.path == "" {
		return nil
	}
	active := make([]terminalBlock, 0)
	for terminal, state := range l.terminals {
		for category, value := range state.blocks {
			if value.until.After(now) {
				active = append(active, terminalBlock{
					terminal: terminal,
					category: category,
					block:    value,
				})
			}
		}
	}
	sort.Slice(active, func(i, j int) bool {
		if active[i].terminal != active[j].terminal {
			return active[i].terminal < active[j].terminal
		}
		return active[i].category < active[j].category
	})

	payload := blocksFile{
		SchemaVersion: blocksSchemaVersion,
		UpdatedAt:     now,
		Blocks:        make(map[string]persistedBlock, len(active)),
	}
	for _, entry := range active {
		payload.Blocks[blockKey(entry.terminal, entry.category)] = persistedBlock{
			Since:    entry.block.since,
			Until:    entry.block.until,
			Restored: entry.block.restored,
		}
	}
	return writeBlocks(l.path, payload)
}

func blockKey(terminal string, category Category) string {
	return terminal + "\x00" + string(category)
}

func parseBlockKey(key string) (string, Category, bool) {
	separator := -1
	for index, r := range key {
		if r == 0 {
			separator = index
			break
		}
	}
	if separator <= 0 || separator == len(key)-1 {
		return "", "", false
	}
	terminal := key[:separator]
	category := Category(key[separator+1:])
	known := false
	for _, value := range Categories() {
		if category == value {
			known = true
			break
		}
	}
	if !known {
		return "", "", false
	}
	return terminal, category, true
}

func writeBlocks(path string, payload blocksFile) error {
	directory := filepath.Dir(path)
	handle, err := os.CreateTemp(directory, ".quota-blocks-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary quota blocks: %w", err)
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
	if err := encoder.Encode(payload); err != nil {
		_ = handle.Close()
		return fmt.Errorf("encode quota blocks: %w", err)
	}
	if err := handle.Sync(); err != nil {
		_ = handle.Close()
		return fmt.Errorf("commit quota blocks bytes: %w", err)
	}
	if err := handle.Close(); err != nil {
		return fmt.Errorf("close temporary quota blocks: %w", err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("replace quota blocks: %w", err)
	}
	removeTemp = false
	return nil
}
