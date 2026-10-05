package workflowcheck

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/ratelimit"
)

type rateLimitErrorView struct {
	Error struct {
		Code              string `json:"code"`
		Detail            string `json:"detail"`
		Terminal          string `json:"terminal"`
		Category          string `json:"category"`
		RetryAfterSeconds int    `json:"retry_after_seconds"`
		WindowStart       string `json:"window_start"`
		WindowEnd         string `json:"window_end"`
		Limit             int    `json:"limit"`
		Used              int    `json:"used"`
	} `json:"error"`
}

type quotaConfigView struct {
	Quotas []struct {
		Category      string `json:"category"`
		Limit         int    `json:"limit"`
		WindowSeconds int    `json:"window_seconds"`
	} `json:"quotas"`
}

// rateLimiting exercises the local quota with real HTTP requests. It covers
// burst handling, window rollover, runtime quota adjustment, restart recovery,
// the read/health bypass, and the guarantee that rejected writes never touch
// business state.
func rateLimiting(dataDir string) error {
	server, err := newCheckServer(dataDir)
	if err != nil {
		return err
	}
	api := newClient(server.url)

	if err := checkDefaultQuotas(api); err != nil {
		return err
	}
	// Tight windows make the check deterministic while staying well above the
	// HTTP client timeout only for the short sleeps below.
	if err := setQuota(api, "formula", 3, 2); err != nil {
		return err
	}
	if err := setQuota(api, "observation", 3, 2); err != nil {
		return err
	}

	if err := checkBurstAndReadBypass(api); err != nil {
		return err
	}
	if err := checkConcurrentBurst(api); err != nil {
		return err
	}
	if err := checkTerminalAndCategoryIsolation(api); err != nil {
		return err
	}
	if err := checkInvalidTerminal(api); err != nil {
		return err
	}
	if err := checkRuntimeQuotaAdjustment(api); err != nil {
		return err
	}
	if err := checkObservationQuotaDoesNotTouchState(api); err != nil {
		return err
	}
	if err := checkWindowRollover(api); err != nil {
		return err
	}
	if err := checkRestartRecovery(dataDir, server); err != nil {
		return err
	}
	return nil
}

func checkDefaultQuotas(api *client) error {
	response, err := api.call("GET", "/v1/quotas", nil, nil)
	if err != nil {
		return err
	}
	if err := requireStatus(response, 200, "read quotas"); err != nil {
		return err
	}
	var view quotaConfigView
	if err := response.decode(&view); err != nil {
		return err
	}
	defaults := map[string]ratelimit.Quota{}
	for _, category := range ratelimit.Categories() {
		defaults[string(category)] = ratelimit.DefaultQuotas()[category]
	}
	if len(view.Quotas) != len(defaults) {
		return errInvalidWorkflow("quotas endpoint did not list every category")
	}
	for _, quota := range view.Quotas {
		expected, found := defaults[quota.Category]
		if !found {
			return errInvalidWorkflow("quotas endpoint returned an unknown category " + quota.Category)
		}
		if quota.Limit != expected.Limit || quota.WindowSeconds != expected.WindowSeconds {
			return errInvalidWorkflow(fmt.Sprintf(
				"default quota for %s was %d/%ds, expected %d/%ds",
				quota.Category, quota.Limit, quota.WindowSeconds,
				expected.Limit, expected.WindowSeconds))
		}
	}
	return nil
}

func setQuota(api *client, category string, limit, windowSeconds int) error {
	response, err := api.call("PUT", "/v1/quotas/"+category, map[string]any{
		"limit":          limit,
		"window_seconds": windowSeconds,
	}, nil)
	if err != nil {
		return err
	}
	if err := requireStatus(response, 200, "set quota "+category); err != nil {
		return err
	}
	return nil
}

func checkBurstAndReadBypass(api *client) error {
	terminal := "tablet-burst"
	for attempt := 1; attempt <= 3; attempt++ {
		response, err := api.callAs("POST", "/v1/formulas",
			burstFormulaPayload(attempt), nil, terminal)
		if err != nil {
			return err
		}
		if err := requireStatus(response, 201, fmt.Sprintf("burst formula create %d", attempt)); err != nil {
			return err
		}
		if err := requireRateHeaders(response, 3, 3-attempt); err != nil {
			return err
		}
	}

	// The fourth and fifth calls in the same window are rejected without any
	// business processing, even though their payloads are valid.
	for attempt := 4; attempt <= 5; attempt++ {
		response, err := api.callAs("POST", "/v1/formulas",
			burstFormulaPayload(attempt), nil, terminal)
		if err != nil {
			return err
		}
		if err := requireCode(response, 429, ratelimit.CodeRateLimited,
			fmt.Sprintf("burst formula create %d", attempt)); err != nil {
			return err
		}
		view, err := decodeRateLimitError(response)
		if err != nil {
			return err
		}
		if view.Error.Terminal != terminal || view.Error.Category != "formula" {
			return errInvalidWorkflow("rate limit response did not identify the terminal and category")
		}
		if view.Error.RetryAfterSeconds < 1 || view.Error.RetryAfterSeconds > 2 {
			return errInvalidWorkflow("rate limit response did not return a clear wait time")
		}
		if got := response.headers.Get("Retry-After"); got != fmt.Sprintf("%d", view.Error.RetryAfterSeconds) {
			return errInvalidWorkflow("Retry-After header did not match the documented wait time")
		}
		if view.Error.WindowStart == "" || view.Error.WindowEnd == "" || view.Error.Limit != 3 || view.Error.Used != 3 {
			return errInvalidWorkflow("rate limit response did not explain the active window")
		}
	}

	// Health and read-only detail/list routes must still answer for a terminal
	// that is currently blocked on writes.
	health, err := api.callAs("GET", "/healthz", nil, nil, terminal)
	if err != nil {
		return err
	}
	if err := requireStatus(health, 200, "health while throttled"); err != nil {
		return err
	}
	list, err := api.callAs("GET", "/v1/formulas", nil, nil, terminal)
	if err != nil {
		return err
	}
	if err := requireStatus(list, 200, "list formulas while throttled"); err != nil {
		return err
	}

	// Only three formulas exist for this terminal; the rejected payloads were
	// not created.
	var created struct {
		Formulas []struct {
			Name string `json:"name"`
		} `json:"formulas"`
	}
	if err := list.decode(&created); err != nil {
		return err
	}
	count := 0
	for _, formula := range created.Formulas {
		if formula.Name == "Burst Four" || formula.Name == "Burst Five" {
			return errInvalidWorkflow("a rejected request modified business state")
		}
		if len(formula.Name) >= 12 && formula.Name[:12] == "Burst Tablet" {
			count++
		}
	}
	if count != 3 {
		return errInvalidWorkflow(fmt.Sprintf("expected 3 accepted burst formulas, found %d", count))
	}
	return nil
}

func checkConcurrentBurst(api *client) error {
	terminal := "tablet-concurrent"
	const goroutines = 10
	var wg sync.WaitGroup
	results := make(chan int, goroutines)
	start := make(chan struct{})
	for index := 0; index < goroutines; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			response, err := api.callAs("POST", "/v1/formulas",
				burstFormulaNamed(fmt.Sprintf("Concurrent Tablet %02d", index)), nil, terminal)
			if err != nil {
				results <- 0
				return
			}
			results <- response.status
		}(index)
	}
	close(start)
	wg.Wait()
	close(results)

	accepted, rejected := 0, 0
	for status := range results {
		switch status {
		case 201:
			accepted++
		case 429:
			rejected++
		default:
			return errInvalidWorkflow(fmt.Sprintf("unexpected concurrent status %d", status))
		}
	}
	if accepted != 3 || rejected != goroutines-3 {
		return errInvalidWorkflow(fmt.Sprintf(
			"concurrent burst produced %d accepted and %d rejected, expected exactly 3/%d",
			accepted, rejected, goroutines-3))
	}
	return nil
}

func checkTerminalAndCategoryIsolation(api *client) error {
	// tablet-burst is blocked on formula writes from the previous scenario; a
	// different terminal must not be.
	other, err := api.callAs("POST", "/v1/formulas",
		burstFormulaNamed("Other Tablet Formula"), nil, "tablet-other")
	if err != nil {
		return err
	}
	if err := requireStatus(other, 201, "other terminal is not blocked"); err != nil {
		return err
	}

	// Quotas are independent per category: the blocked formula terminal can
	// still write in the run category (its setup requests later prove this).
	vessel, err := createCheckVesselAs(api, "VSL-RATE-1", 250, "tablet-burst")
	if err != nil {
		return err
	}
	formula := latestApprovedFormulaForCategoryCheck(api)
	run, err := api.callAs("POST", "/v1/runs",
		createRunPayload("RUN-RATE-ISOLATION", formula.id), nil, "tablet-burst")
	if err != nil {
		return err
	}
	if err := requireStatus(run, 201, "blocked formula terminal still has run quota"); err != nil {
		return err
	}
	_ = vessel
	return nil
}

func checkInvalidTerminal(api *client) error {
	response, err := api.callAs("POST", "/v1/formulas",
		burstFormulaPayload(1), nil, "bad terminal id with spaces")
	if err != nil {
		return err
	}
	if err := requireCode(response, 400, "invalid_input", "invalid terminal header"); err != nil {
		return err
	}
	return nil
}

func checkRuntimeQuotaAdjustment(api *client) error {
	terminal := "tablet-adjust"
	for attempt := 1; attempt <= 3; attempt++ {
		response, err := api.callAs("POST", "/v1/formulas",
			burstFormulaNamed(fmt.Sprintf("Adjust Tablet %d", attempt)), nil, terminal)
		if err != nil {
			return err
		}
		if err := requireStatus(response, 201, fmt.Sprintf("adjust window create %d", attempt)); err != nil {
			return err
		}
	}
	blocked, err := api.callAs("POST", "/v1/formulas",
		burstFormulaNamed("Adjust Tablet Blocked"), nil, terminal)
	if err != nil {
		return err
	}
	if err := requireCode(blocked, 429, ratelimit.CodeRateLimited, "blocked before adjustment"); err != nil {
		return err
	}

	// Raising the quota at runtime must not retroactively widen a window that
	// is already running: this terminal still sees exactly one answer per
	// request until the window it is inside elapses.
	if err := setQuota(api, "formula", 50, 2); err != nil {
		return err
	}
	stillBlocked, err := api.callAs("POST", "/v1/formulas",
		burstFormulaNamed("Adjust Tablet Still Blocked"), nil, terminal)
	if err != nil {
		return err
	}
	if err := requireCode(stillBlocked, 429, ratelimit.CodeRateLimited,
		"running window keeps its original limit"); err != nil {
		return err
	}

	// A different terminal opening its first window after the change receives
	// the new quota.
	fresh, err := api.callAs("POST", "/v1/formulas",
		burstFormulaNamed("Fresh Tablet After Adjust 1"), nil, "tablet-adjust-fresh")
	if err != nil {
		return err
	}
	if err := requireStatus(fresh, 201, "fresh terminal uses raised quota"); err != nil {
		return err
	}
	if got := fresh.headers.Get("X-RateLimit-Limit"); got != "50" {
		return errInvalidWorkflow("raised quota was not announced to new windows")
	}

	// Invalid runtime updates are rejected.
	bad, err := api.call("PUT", "/v1/quotas/formula", map[string]any{
		"limit": 0, "window_seconds": 2,
	}, nil)
	if err != nil {
		return err
	}
	if err := requireCode(bad, 400, "invalid_input", "reject zero quota"); err != nil {
		return err
	}
	unknown, err := api.call("PUT", "/v1/quotas/nope", map[string]any{
		"limit": 1, "window_seconds": 2,
	}, nil)
	if err != nil {
		return err
	}
	if err := requireCode(unknown, 400, "invalid_input", "reject unknown category"); err != nil {
		return err
	}
	return nil
}

func checkObservationQuotaDoesNotTouchState(api *client) error {
	formula, err := ensureApprovedFormulaAs(api, "Observation Quota Formula", "tablet-obs-setup")
	if err != nil {
		return err
	}
	vessel, err := createCheckVesselAs(api, "VSL-RATE-OBS", 250, "tablet-obs-setup")
	if err != nil {
		return err
	}
	started, err := createStartedRunAs(api, formula.ID, vessel.ID,
		"RUN-RATE-OBS", "tablet-obs-setup")
	if err != nil {
		return err
	}

	terminal := "tablet-obs"
	base := time.Now().UTC().Add(10 * time.Second)
	version := started.Run.Version
	for sequence := 1; sequence <= 3; sequence++ {
		response, err := api.callAs("POST", "/v1/runs/"+started.Run.ID+"/observations",
			observationPayload(base, sequence, 1.05-float64(sequence)*0.01),
			ptrInt64(version), terminal)
		if err != nil {
			return err
		}
		if err := requireStatus(response, 200, fmt.Sprintf("accepted observation %d", sequence)); err != nil {
			return err
		}
		var run runView
		if err := response.decode(&run); err != nil {
			return err
		}
		version = run.Version
	}

	denied, err := api.callAs("POST", "/v1/runs/"+started.Run.ID+"/observations",
		observationPayload(base, 4, 1.005), ptrInt64(version), terminal)
	if err != nil {
		return err
	}
	if err := requireCode(denied, 429, ratelimit.CodeRateLimited,
		"fourth observation rejected by observation category quota"); err != nil {
		return err
	}

	// The run must be unchanged: three observations, still fermenting, same
	// revision. A read route verifies this even while the terminal is blocked.
	readResponse, err := api.callAs("GET", "/v1/runs/"+started.Run.ID, nil, nil, terminal)
	if err != nil {
		return err
	}
	if err := requireStatus(readResponse, 200, "read run while observation quota blocked"); err != nil {
		return err
	}
	var run runView
	if err := readResponse.decode(&run); err != nil {
		return err
	}
	if len(run.Observations) != 3 || run.Version != version {
		return errInvalidWorkflow("a rejected observation modified the run")
	}
	return nil
}

func checkWindowRollover(api *client) error {
	terminal := "tablet-rollover"
	if err := setQuota(api, "formula", 2, 2); err != nil {
		return err
	}
	first, err := api.callAs("POST", "/v1/formulas",
		burstFormulaNamed("Rollover Tablet 1"), nil, terminal)
	if err != nil {
		return err
	}
	if err := requireStatus(first, 201, "rollover create first"); err != nil {
		return err
	}
	second, err := api.callAs("POST", "/v1/formulas",
		burstFormulaNamed("Rollover Tablet 2"), nil, terminal)
	if err != nil {
		return err
	}
	if err := requireStatus(second, 201, "rollover create second"); err != nil {
		return err
	}
	denied, err := api.callAs("POST", "/v1/formulas",
		burstFormulaNamed("Rollover Tablet Denied"), nil, terminal)
	if err != nil {
		return err
	}
	if err := requireCode(denied, 429, ratelimit.CodeRateLimited, "denied at end of window"); err != nil {
		return err
	}

	// Cross the window boundary. The next request opens a fresh window and gets
	// exactly one new verdict (accepted), with the counter reset to one used.
	time.Sleep(2100 * time.Millisecond)
	after, err := api.callAs("POST", "/v1/formulas",
		burstFormulaNamed("Rollover Tablet New Window"), nil, terminal)
	if err != nil {
		return err
	}
	if err := requireStatus(after, 201, "accepted after window rollover"); err != nil {
		return err
	}
	if err := requireRateHeaders(after, 2, 1); err != nil {
		return err
	}

	// Restore a small quota so the restart scenario below is deterministic.
	if err := setQuota(api, "observation", 3, 2); err != nil {
		return err
	}
	return nil
}

func checkRestartRecovery(dataDir string, old *checkServer) error {
	// The observation terminal used above is still inside its 2-second blocked
	// window. Close the controller and reopen it against the same directory.
	old.close()

	reopened, err := newCheckServer(dataDir)
	if err != nil {
		return err
	}
	defer reopened.close()
	api := newClient(reopened.url)

	// Health and reads answer immediately, with an explainable service status.
	health, err := api.call("GET", "/healthz", nil, nil)
	if err != nil {
		return err
	}
	if err := requireStatus(health, 200, "health after restart"); err != nil {
		return err
	}
	quotas, err := api.call("GET", "/v1/quotas", nil, nil)
	if err != nil {
		return err
	}
	if err := requireStatus(quotas, 200, "quotas survived restart"); err != nil {
		return err
	}
	var view quotaConfigView
	if err := quotas.decode(&view); err != nil {
		return err
	}
	for _, quota := range view.Quotas {
		switch quota.Category {
		case "formula":
			if quota.Limit != 2 || quota.WindowSeconds != 2 {
				return errInvalidWorkflow("formula quota adjustment did not survive restart")
			}
		case "observation":
			if quota.Limit != 3 || quota.WindowSeconds != 2 {
				return errInvalidWorkflow("observation quota adjustment did not survive restart")
			}
		}
	}

	// A terminal still inside its limiting window receives the same explainable
	// rejection after restart instead of silently getting a fresh counter.
	denied, err := api.callAs("POST", "/v1/runs/run_does_not_matter/observations",
		observationPayload(time.Now().UTC(), 9, 1.0), nil, "tablet-obs")
	if err != nil {
		return err
	}
	if err := requireCode(denied, 429, ratelimit.CodeRateLimited,
		"terminal still blocked after controller restart"); err != nil {
		return err
	}
	explained, err := decodeRateLimitError(denied)
	if err != nil {
		return err
	}
	if explained.Error.Category != "observation" ||
		explained.Error.Used != 3 || explained.Error.Limit != 3 ||
		explained.Error.RetryAfterSeconds < 1 {
		return errInvalidWorkflow("restart rejection was not explainable from persisted state")
	}

	// Once that window has elapsed the terminal is served again, and a window
	// that was already over at startup never blocks anyone.
	time.Sleep(2200 * time.Millisecond)
	formula, err := ensureApprovedFormulaAs(api, "Recovery Formula", "tablet-obs")
	if err != nil {
		return err
	}
	vessel, err := createCheckVesselAs(api, "VSL-RATE-REC", 250, "tablet-obs")
	if err != nil {
		return err
	}
	started, err := createStartedRunAs(api, formula.ID, vessel.ID, "RUN-RATE-REC", "tablet-obs")
	if err != nil {
		return err
	}
	response, err := api.callAs("POST", "/v1/runs/"+started.Run.ID+"/observations",
		observationPayload(time.Now().UTC().Add(10*time.Second), 1, 1.045),
		nil, "tablet-obs")
	if err != nil {
		return err
	}
	if err := requireStatus(response, 200, "terminal served after persisted window elapsed"); err != nil {
		return err
	}
	return nil
}

func requireRateHeaders(response response, limit, remaining int) error {
	if got := response.headers.Get("X-RateLimit-Limit"); got != fmt.Sprintf("%d", limit) {
		return errInvalidWorkflow("X-RateLimit-Limit was " + got)
	}
	if got := response.headers.Get("X-RateLimit-Remaining"); got != fmt.Sprintf("%d", remaining) {
		return errInvalidWorkflow("X-RateLimit-Remaining was " + got)
	}
	if response.headers.Get("X-RateLimit-Reset") == "" {
		return errInvalidWorkflow("X-RateLimit-Reset was missing")
	}
	return nil
}

func decodeRateLimitError(response response) (rateLimitErrorView, error) {
	var view rateLimitErrorView
	if err := json.Unmarshal(response.body, &view); err != nil {
		return rateLimitErrorView{}, fmt.Errorf("decode rate limit response: %w", err)
	}
	return view, nil
}

func burstFormulaPayload(attempt int) map[string]any {
	names := map[int]string{
		1: "Burst Tablet One",
		2: "Burst Tablet Two",
		3: "Burst Tablet Three",
		4: "Burst Four",
		5: "Burst Five",
	}
	return burstFormulaNamed(names[attempt])
}

func burstFormulaNamed(name string) map[string]any {
	payload := createFormulaPayload(name)
	return payload
}

func observationPayload(base time.Time, sequence int, gravity float64) map[string]any {
	return map[string]any{
		"sequence":      sequence,
		"gravity":       gravity,
		"temperature_c": 20.0,
		"observed_at":   verifiedAt(base, sequence),
		"note":          fmt.Sprintf("rate observation %d", sequence),
	}
}

var _ = http.StatusTooManyRequests
