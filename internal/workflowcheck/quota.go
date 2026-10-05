package workflowcheck

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/domain"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/ratelimit"
)

// quotaEnforcement drives the real HTTP handler with a fixed clock to verify:
// burst blocking with an explicit wait, read/health exemption, per-terminal
// and per-category isolation, window rollover, that rejected requests never
// reach business state, runtime quota adjustment, concurrency linearizability,
// and explainable recovery after a controller restart.
func quotaEnforcement(env *checkEnv, harness *checkHarness) error {
	api, clk := env.api, env.clk
	const burstTerminal = "tablet-burst"
	const neighborTerminal = "tablet-neighbor"

	// Set up one approved formula so later observation requests are well
	// formed and would change state if the limiter failed to stop them.
	formula, err := ensureApprovedFormula(api, "Quota Formula")
	if err != nil {
		return err
	}
	createdCount := 1

	// Five formula writes from one terminal fit the limit.
	for attempt := 1; attempt <= 5; attempt++ {
		response, err := api.call("POST", "/v1/formulas",
			createFormulaPayload(fmt.Sprintf("Burst Formula %d", attempt)),
			nil, terminalHeader(burstTerminal))
		if err != nil {
			return err
		}
		if err := requireStatus(response, 201, fmt.Sprintf("burst write %d", attempt)); err != nil {
			return err
		}
		createdCount++
	}

	// The sixth is a burst and must be rejected with an explicit wait.
	rejected, err := api.call("POST", "/v1/formulas",
		createFormulaPayload("Burst Formula Rejected"), nil, terminalHeader(burstTerminal))
	if err != nil {
		return err
	}
	if err := requireCode(rejected, 429, string(domain.CodeQuotaExceeded), "burst rejection"); err != nil {
		return err
	}
	if value, err := rejected.retryAfter(); err != nil || value != "120" {
		return fmt.Errorf("expected Retry-After 120 on burst, got %q", value)
	}

	// Rejected requests must never touch business state.
	list, err := api.call("GET", "/v1/formulas", nil, nil, terminalHeader(burstTerminal))
	if err != nil {
		return err
	}
	var listBody struct {
		Formulas []struct {
			Name string `json:"name"`
		} `json:"formulas"`
	}
	if err := requireStatus(list, 200, "list formulas while blocked"); err != nil {
		return err
	}
	if err := list.decode(&listBody); err != nil {
		return err
	}
	if len(listBody.Formulas) != createdCount {
		return fmt.Errorf("rejected write changed business state: got %d formulas, want %d",
			len(listBody.Formulas), createdCount)
	}
	for _, item := range listBody.Formulas {
		if item.Name == "Burst Formula Rejected" {
			return errInvalidWorkflow("rejected request created a formula")
		}
	}

	// Health checks and read-only details stay available from the blocked
	// terminal itself.
	health, err := api.call("GET", "/healthz", nil, nil, terminalHeader(burstTerminal))
	if err != nil {
		return err
	}
	if err := requireStatus(health, 200, "health while blocked"); err != nil {
		return err
	}
	detail, err := api.call("GET", "/v1/formulas/"+formula.ID, nil, nil, terminalHeader(burstTerminal))
	if err != nil {
		return err
	}
	if err := requireStatus(detail, 200, "read-only detail while blocked"); err != nil {
		return err
	}

	// Quota status explains the block, including a concrete wait.
	status, err := api.call("GET", "/quota-status", nil, nil, terminalHeader(burstTerminal))
	if err != nil {
		return err
	}
	if err := requireStatus(status, 200, "quota status while blocked"); err != nil {
		return err
	}
	var statusBody struct {
		Terminal   string `json:"terminal"`
		Categories map[string]struct {
			Blocked           bool `json:"blocked"`
			Remaining         int  `json:"remaining"`
			RetryAfterSeconds int  `json:"retry_after_seconds"`
		} `json:"categories"`
	}
	if err := status.decode(&statusBody); err != nil {
		return err
	}
	formulaStatus := statusBody.Categories["formula"]
	if !formulaStatus.Blocked || formulaStatus.Remaining != 0 || formulaStatus.RetryAfterSeconds != 120 {
		return fmt.Errorf("quota status did not explain the active formula block: %+v", formulaStatus)
	}

	// Another terminal is independent and not blocked.
	neighbor, err := api.call("POST", "/v1/formulas",
		createFormulaPayload("Neighbor Formula"), nil, terminalHeader(neighborTerminal))
	if err != nil {
		return err
	}
	if err := requireStatus(neighbor, 201, "neighbor terminal write"); err != nil {
		return err
	}
	createdCount++

	// Categories are independent: the blocked terminal can still write to
	// another category.
	vesselResponse, err := api.call("POST", "/v1/vessels",
		map[string]any{"code": "VSL-Q1", "capacity_liters": 500},
		nil, terminalHeader(burstTerminal))
	if err != nil {
		return err
	}
	if err := requireStatus(vesselResponse, 201, "vessel write from formula-blocked terminal"); err != nil {
		return err
	}
	var quotaVessel vesselView
	if err := vesselResponse.decode(&quotaVessel); err != nil {
		return err
	}

	// While blocked, crossing part of the penalty changes nothing.
	clk.Advance(30 * time.Second)
	stillBlocked, err := api.call("POST", "/v1/formulas",
		createFormulaPayload("Still Blocked"), nil, terminalHeader(burstTerminal))
	if err != nil {
		return err
	}
	if err := requireCode(stillBlocked, 429, string(domain.CodeQuotaExceeded),
		"still blocked within penalty"); err != nil {
		return err
	}
	if value, err := stillBlocked.retryAfter(); err != nil || value != "90" {
		return fmt.Errorf("expected Retry-After 90 mid-penalty, got %q", value)
	}

	// Cross the penalty boundary: the next write is admitted and starts a
	// fresh window.
	clk.Advance(91 * time.Second)
	admitted, err := api.call("POST", "/v1/formulas",
		createFormulaPayload("Recovered Formula"), nil, terminalHeader(burstTerminal))
	if err != nil {
		return err
	}
	if err := requireStatus(admitted, 201, "write after penalty elapsed"); err != nil {
		return err
	}
	createdCount++

	// Four more accepted writes fill the single fresh window; a fifth then
	// trips a new block, exercising pure window rollover without a restart.
	for attempt := 2; attempt <= 5; attempt++ {
		response, err := api.call("POST", "/v1/formulas",
			createFormulaPayload(fmt.Sprintf("Window Formula %d", attempt)),
			nil, terminalHeader(burstTerminal))
		if err != nil {
			return err
		}
		if err := requireStatus(response, 201, fmt.Sprintf("window fill %d", attempt)); err != nil {
			return err
		}
		createdCount++
	}
	overflow, err := api.call("POST", "/v1/formulas",
		createFormulaPayload("Window Overflow"), nil, terminalHeader(burstTerminal))
	if err != nil {
		return err
	}
	if err := requireCode(overflow, 429, string(domain.CodeQuotaExceeded),
		"overflow after sliding window filled"); err != nil {
		return err
	}

	// Runtime adjustment: an invalid change is rejected and leaves the running
	// rules in force, and raising a limit never silently erases an active
	// penalty — either outcome is a single consistent decision.
	if err := harness.limiter.UpdateConfig(ratelimit.Config{
		Rules: map[ratelimit.Category]ratelimit.CategoryRule{
			ratelimit.CategoryFormula:     {Limit: 0, Window: time.Minute},
			ratelimit.CategoryVessel:      {Limit: 5, Window: time.Minute},
			ratelimit.CategoryRun:         {Limit: 5, Window: time.Minute},
			ratelimit.CategoryObservation: {Limit: 5, Window: time.Minute},
		},
		Penalty: 2 * time.Minute,
	}); err == nil {
		return errInvalidWorkflow("invalid runtime quota adjustment was accepted")
	}
	if err := harness.limiter.UpdateConfig(withLimit(quotaConfig(), ratelimit.CategoryFormula, 100)); err != nil {
		return err
	}
	stillPenalized, err := api.call("POST", "/v1/formulas",
		createFormulaPayload("Raise Does Not Clear Penalty"), nil, terminalHeader(burstTerminal))
	if err != nil {
		return err
	}
	if err := requireCode(stillPenalized, 429, string(domain.CodeQuotaExceeded),
		"active penalty survives a limit raise"); err != nil {
		return err
	}
	// A fresh terminal immediately observes the raised limit.
	const tuningTerminal = "tablet-tuning"
	for attempt := 1; attempt <= 3; attempt++ {
		response, err := api.call("POST", "/v1/formulas",
			createFormulaPayload(fmt.Sprintf("Tuning Formula %d", attempt)),
			nil, terminalHeader(tuningTerminal))
		if err != nil {
			return err
		}
		if err := requireStatus(response, 201, fmt.Sprintf("write under raised limit %d", attempt)); err != nil {
			return err
		}
		createdCount++
	}
	if err := harness.limiter.UpdateConfig(quotaConfig()); err != nil {
		return err
	}

	// Observation category is counted independently.
	const obsTerminal = "tablet-observer"
	started, err := createStartedRun(api, formula.ID, quotaVessel.ID, "RUN-QUOTA")
	if err != nil {
		return err
	}
	base := clk.Now().Add(10 * time.Second)
	for sequence := 1; sequence <= 5; sequence++ {
		response, err := api.call("POST", "/v1/runs/"+started.Run.ID+"/observations",
			map[string]any{
				"sequence":      sequence,
				"gravity":       1.050 - float64(sequence)*0.01,
				"temperature_c": 20.0,
				"observed_at":   verifiedAt(base, sequence),
				"note":          "quota observation",
			}, nil, terminalHeader(obsTerminal))
		if err != nil {
			return err
		}
		if err := requireStatus(response, 200, fmt.Sprintf("observation %d", sequence)); err != nil {
			return err
		}
	}
	sixthObservation, err := api.call("POST", "/v1/runs/"+started.Run.ID+"/observations",
		map[string]any{
			"sequence":      6,
			"gravity":       0.999,
			"temperature_c": 20.0,
			"observed_at":   verifiedAt(base, 6),
			"note":          "blocked observation",
		}, nil, terminalHeader(obsTerminal))
	if err != nil {
		return err
	}
	if err := requireCode(sixthObservation, 429, string(domain.CodeQuotaExceeded),
		"sixth observation blocked"); err != nil {
		return err
	}
	// The blocked observation did not advance the run's observation history.
	readRun, err := api.call("GET", "/v1/runs/"+started.Run.ID, nil, nil)
	if err != nil {
		return err
	}
	var runState runView
	if err := readRun.decode(&runState); err != nil {
		return err
	}
	if len(runState.Observations) != 5 {
		return fmt.Errorf("blocked observation touched business state: got %d observations",
			len(runState.Observations))
	}

	// A concurrent burst against a fresh terminal yields exactly one result
	// per request: precisely Limit accepts and the rest 429s.
	const concurrentTerminal = "tablet-race"
	var wg sync.WaitGroup
	results := make([]int, 20)
	for index := range results {
		wg.Add(1)
		go func(position int) {
			defer wg.Done()
			response, err := api.call("POST", "/v1/formulas",
				createFormulaPayload(fmt.Sprintf("Concurrent Formula %02d", position)),
				nil, terminalHeader(concurrentTerminal))
			if err != nil {
				results[position] = -1
				return
			}
			results[position] = response.status
		}(index)
	}
	wg.Wait()
	sort.Ints(results)
	accepted, blockedConcurrency := 0, 0
	for _, statusCode := range results {
		switch statusCode {
		case 201:
			accepted++
		case 429:
			blockedConcurrency++
		default:
			return fmt.Errorf("concurrent burst returned unexpected status %d", statusCode)
		}
	}
	if accepted != 5 || blockedConcurrency != 15 {
		return fmt.Errorf("concurrent burst gave %d accepts and %d rejections, want 5/15",
			accepted, blockedConcurrency)
	}
	createdCount += 5

	// Controller restart: a terminal still inside its penalty window stays
	// blocked with an explainable, restored response; waits are honored.
	const restartTerminal = "tablet-restart"
	for attempt := 1; attempt <= 5; attempt++ {
		response, err := api.call("POST", "/v1/formulas",
			createFormulaPayload(fmt.Sprintf("Restart Formula %d", attempt)),
			nil, terminalHeader(restartTerminal))
		if err != nil {
			return err
		}
		if err := requireStatus(response, 201, fmt.Sprintf("restart setup write %d", attempt)); err != nil {
			return err
		}
		createdCount++
	}
	blockBeforeRestart, err := api.call("POST", "/v1/formulas",
		createFormulaPayload("Restart Blocked"), nil, terminalHeader(restartTerminal))
	if err != nil {
		return err
	}
	if err := requireCode(blockBeforeRestart, 429, string(domain.CodeQuotaExceeded),
		"block before restart"); err != nil {
		return err
	}

	clk.Advance(10 * time.Second)
	if err := harness.reopen(); err != nil {
		return err
	}
	restartAPI := harness.client()

	afterRestart, err := restartAPI.call("POST", "/v1/formulas",
		createFormulaPayload("After Restart"), nil, terminalHeader(restartTerminal))
	if err != nil {
		return err
	}
	if err := requireCode(afterRestart, 429, string(domain.CodeQuotaExceeded),
		"block survives restart within window"); err != nil {
		return err
	}
	if value, err := afterRestart.retryAfter(); err != nil || value != "110" {
		return fmt.Errorf("expected Retry-After 110 after restart, got %q", value)
	}
	var restoredBody struct {
		Restored bool `json:"restart_restored"`
	}
	if err := afterRestart.decode(&restoredBody); err != nil {
		return err
	}
	if !restoredBody.Restored {
		return errInvalidWorkflow("post-restart block was not marked as restored")
	}
	restoredStatus, err := restartAPI.call("GET", "/quota-status", nil, nil,
		terminalHeader(restartTerminal))
	if err != nil {
		return err
	}
	if err := requireStatus(restoredStatus, 200, "quota status after restart"); err != nil {
		return err
	}

	// Once the restored penalty elapses the terminal is admitted again.
	clk.Advance(111 * time.Second)
	recovered, err := restartAPI.call("POST", "/v1/formulas",
		createFormulaPayload("Post Restart Recovery"), nil, terminalHeader(restartTerminal))
	if err != nil {
		return err
	}
	if err := requireStatus(recovered, 201, "write after restored penalty elapsed"); err != nil {
		return err
	}
	createdCount++

	// Final invariant: only accepted writes ever created formulas.
	finalList, err := restartAPI.call("GET", "/v1/formulas", nil, nil)
	if err != nil {
		return err
	}
	var finalFormulas struct {
		Formulas []any `json:"formulas"`
	}
	if err := finalList.decode(&finalFormulas); err != nil {
		return err
	}
	if len(finalFormulas.Formulas) != createdCount {
		return fmt.Errorf("final formula count %d does not match accepted writes %d",
			len(finalFormulas.Formulas), createdCount)
	}
	return nil
}

func withLimit(cfg ratelimit.Config, category ratelimit.Category, limit int) ratelimit.Config {
	rule := cfg.Rules[category]
	rule.Limit = limit
	cfg.Rules[category] = rule
	return cfg
}
