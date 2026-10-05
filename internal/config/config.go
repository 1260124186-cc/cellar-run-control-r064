package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/ratelimit"
)

const (
	DefaultAddress = "127.0.0.1:8088"
	DefaultDataDir = "./data"
)

// Config contains process-level settings that are resolved before any
// repository or HTTP server is created.
type Config struct {
	Address           string
	DataDir           string
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
	QuotaEnabled      bool
	Quota             ratelimit.Config
}

func FromEnv() (Config, error) {
	window, err := durationEnv("CELLAR_CONTROL_QUOTA_WINDOW", time.Minute)
	if err != nil {
		return Config{}, err
	}
	penalty, err := durationEnv("CELLAR_CONTROL_QUOTA_PENALTY", 2*time.Minute)
	if err != nil {
		return Config{}, err
	}
	limits := map[ratelimit.Category]int{
		ratelimit.CategoryFormula:     30,
		ratelimit.CategoryVessel:      30,
		ratelimit.CategoryRun:         30,
		ratelimit.CategoryObservation: 60,
	}
	for category := range limits {
		name := "CELLAR_CONTROL_QUOTA_LIMIT_" + strings.ToUpper(string(category))
		value, err := intEnv(name, limits[category])
		if err != nil {
			return Config{}, err
		}
		limits[category] = value
	}
	cfg := Config{
		Address:           valueOrDefault("CELLAR_CONTROL_ADDR", DefaultAddress),
		DataDir:           valueOrDefault("CELLAR_CONTROL_DATA_DIR", DefaultDataDir),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       60 * time.Second,
		ShutdownTimeout:   10 * time.Second,
		QuotaEnabled:      boolEnv("CELLAR_CONTROL_QUOTA_ENABLED", true),
		Quota: ratelimit.Config{
			Penalty: penalty,
			Rules: map[ratelimit.Category]ratelimit.CategoryRule{
				ratelimit.CategoryFormula:     {Limit: limits[ratelimit.CategoryFormula], Window: window},
				ratelimit.CategoryVessel:      {Limit: limits[ratelimit.CategoryVessel], Window: window},
				ratelimit.CategoryRun:         {Limit: limits[ratelimit.CategoryRun], Window: window},
				ratelimit.CategoryObservation: {Limit: limits[ratelimit.CategoryObservation], Window: window},
			},
		},
	}
	// An optional JSON file overrides the environment-derived quota rules.
	if quotaFile := valueOrDefault("CELLAR_CONTROL_QUOTA_FILE", ""); quotaFile != "" {
		fileQuota, err := LoadQuotaFile(quotaFile)
		if err != nil {
			return Config{}, err
		}
		cfg.Quota = fileQuota
	}
	return cfg.Validate()
}

func (c Config) Validate() (Config, error) {
	c.Address = strings.TrimSpace(c.Address)
	c.DataDir = strings.TrimSpace(c.DataDir)
	if c.Address == "" {
		return Config{}, fmt.Errorf("address cannot be empty")
	}
	if c.DataDir == "" {
		return Config{}, fmt.Errorf("data directory cannot be empty")
	}
	absolute, err := filepath.Abs(c.DataDir)
	if err != nil {
		return Config{}, fmt.Errorf("resolve data directory: %w", err)
	}
	c.DataDir = filepath.Clean(absolute)
	if c.ReadHeaderTimeout <= 0 || c.ReadTimeout <= 0 || c.WriteTimeout <= 0 {
		return Config{}, fmt.Errorf("HTTP timeouts must be positive")
	}
	if c.IdleTimeout <= 0 || c.ShutdownTimeout <= 0 {
		return Config{}, fmt.Errorf("idle and shutdown timeouts must be positive")
	}
	if c.QuotaEnabled {
		validated, err := c.Quota.Validated()
		if err != nil {
			return Config{}, err
		}
		c.Quota = validated
	}
	return c, nil
}

func (c Config) EnsureDataDir() error {
	if err := os.MkdirAll(c.DataDir, 0o755); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	return nil
}

// ReloadQuota reads the currently configured quota file so a running process
// can swap rules on SIGHUP. It returns an error when no file is configured.
func ReloadQuota() (ratelimit.Config, string, error) {
	path := valueOrDefault("CELLAR_CONTROL_QUOTA_FILE", "")
	if path == "" {
		return ratelimit.Config{}, "", fmt.Errorf("CELLAR_CONTROL_QUOTA_FILE is not set")
	}
	cfg, err := LoadQuotaFile(path)
	if err != nil {
		return ratelimit.Config{}, path, err
	}
	return cfg, path, nil
}

func valueOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func durationEnv(name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration such as 60s: %w", name, err)
	}
	return parsed, nil
}

func intEnv(name string, fallback int) (int, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", name, err)
	}
	return parsed, nil
}

func boolEnv(name string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "":
		return fallback
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}
