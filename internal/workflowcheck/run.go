package workflowcheck

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/clock"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/httpapi"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/ratelimit"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/service"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/storage"
)

// checkHarness owns one data directory and the fixed clock shared by the
// business service and the quota limiter, so time-dependent quota behavior is
// fully deterministic.
type checkHarness struct {
	dataDir string
	clk     clock.Clock
	server  *httptest.Server
	limiter *ratelimit.Limiter
}

// checkEnv is what a workflow needs to drive the service.
type checkEnv struct {
	api     *client
	clk     *clock.Fixed
	dataDir string
}

// quotaConfig returns tight, deterministic rules for the quota enforcement
// check. Every category shares a one-minute window and a two-minute penalty;
// the small limits let a real burst trip the block quickly.
func quotaConfig() ratelimit.Config {
	return ratelimit.Config{
		Rules: map[ratelimit.Category]ratelimit.CategoryRule{
			ratelimit.CategoryFormula:     {Limit: 5, Window: time.Minute},
			ratelimit.CategoryVessel:      {Limit: 5, Window: time.Minute},
			ratelimit.CategoryRun:         {Limit: 5, Window: time.Minute},
			ratelimit.CategoryObservation: {Limit: 5, Window: time.Minute},
		},
		Penalty: 2 * time.Minute,
	}
}

// generousQuotaConfig keeps the three original workflow checks far from any
// limit while still exercising the gating middleware.
func generousQuotaConfig() ratelimit.Config {
	config := quotaConfig()
	for category := range config.Rules {
		config.Rules[category] = ratelimit.CategoryRule{Limit: 1000, Window: time.Minute}
	}
	return config
}

func startHarness(dataDir string, clk clock.Clock, quotaCfg ratelimit.Config) (*checkHarness, error) {
	store, err := storage.Open(dataDir)
	if err != nil {
		return nil, err
	}
	app := service.New(store, clk)
	limiter, err := ratelimit.Open(
		filepath.Join(dataDir, "quota-blocks.json"), quotaCfg, clk)
	if err != nil {
		return nil, err
	}
	server := httptest.NewServer(httpapi.NewHandler(app, limiter))
	return &checkHarness{dataDir: dataDir, clk: clk, server: server, limiter: limiter}, nil
}

func (h *checkHarness) client() *client {
	return newClient(h.server.URL)
}

func (h *checkHarness) close() {
	h.server.Close()
}

// reopen simulates a controller restart against the same data directory: a
// fresh store and limiter reload the durable snapshots and blocks.
func (h *checkHarness) reopen() error {
	h.close()
	restarted, err := startHarness(h.dataDir, h.clk, quotaConfig())
	if err != nil {
		return err
	}
	h.server = restarted.server
	return nil
}

func Run(name string) error {
	dataDir, err := os.MkdirTemp("", "cellar-run-check-*")
	if err != nil {
		return fmt.Errorf("create check directory: %w", err)
	}
	defer os.RemoveAll(dataDir)

	start := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)

	switch name {
	case "formula-approval":
		harness, err := startHarness(dataDir, clock.System{}, generousQuotaConfig())
		if err != nil {
			return err
		}
		defer harness.close()
		if err := formulaApproval(harness.client()); err != nil {
			return err
		}
	case "vessel-run-lifecycle":
		harness, err := startHarness(dataDir, clock.System{}, generousQuotaConfig())
		if err != nil {
			return err
		}
		defer harness.close()
		if err := vesselRunLifecycle(harness.client()); err != nil {
			return err
		}
	case "observations-completion":
		harness, err := startHarness(dataDir, clock.System{}, generousQuotaConfig())
		if err != nil {
			return err
		}
		defer harness.close()
		if err := observationsCompletion(harness.client()); err != nil {
			return err
		}
	case "quota-enforcement":
		clk := clock.NewFixed(start)
		harness, err := startHarness(dataDir, clk, quotaConfig())
		if err != nil {
			return err
		}
		defer harness.close()
		env := &checkEnv{api: harness.client(), clk: clk, dataDir: dataDir}
		if err := quotaEnforcement(env, harness); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown workflow check %q", name)
	}
	fmt.Printf("workflow check passed: %s\n", name)
	return nil
}
