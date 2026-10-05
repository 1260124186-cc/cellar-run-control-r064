package config

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/ratelimit"
)

// quotaFile is the optional on-disk representation of quota rules. It exists so
// limits can be adjusted on a running controller with SIGHUP without restarting
// the process or editing business state.
//
// Example:
//
//	{
//	  "penalty": "2m",
//	  "rules": {
//	    "formula":     {"limit": 10, "window": "60s"},
//	    "vessel":      {"limit": 10, "window": "60s"},
//	    "run":         {"limit": 10, "window": "60s"},
//	    "observation": {"limit": 30, "window": "60s"}
//	  }
//	}
type quotaFile struct {
	Penalty string                   `json:"penalty"`
	Rules   map[string]quotaFileRule `json:"rules"`
}

type quotaFileRule struct {
	Limit  int    `json:"limit"`
	Window string `json:"window"`
}

// LoadQuotaFile reads and validates the optional quota configuration file.
func LoadQuotaFile(path string) (ratelimit.Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ratelimit.Config{}, fmt.Errorf("read quota file: %w", err)
	}
	var parsed quotaFile
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ratelimit.Config{}, fmt.Errorf("decode quota file: %w", err)
	}
	penalty, err := time.ParseDuration(parsed.Penalty)
	if err != nil {
		return ratelimit.Config{}, fmt.Errorf("quota file penalty must be a duration: %w", err)
	}
	cfg := ratelimit.Config{Penalty: penalty, Rules: map[ratelimit.Category]ratelimit.CategoryRule{}}
	for _, category := range ratelimit.Categories() {
		rule, ok := parsed.Rules[string(category)]
		if !ok {
			return ratelimit.Config{}, fmt.Errorf("quota file is missing rule %q", category)
		}
		window, err := time.ParseDuration(rule.Window)
		if err != nil {
			return ratelimit.Config{}, fmt.Errorf("quota file window for %q must be a duration: %w", category, err)
		}
		cfg.Rules[category] = ratelimit.CategoryRule{Limit: rule.Limit, Window: window}
	}
	validated, err := cfg.Validated()
	if err != nil {
		return ratelimit.Config{}, err
	}
	return validated, nil
}
