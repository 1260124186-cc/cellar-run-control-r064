package workflowcheck

import (
	"fmt"
	"net/http/httptest"
	"os"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/clock"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/httpapi"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/ratelimit"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/service"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/storage"
)

func Run(name string) error {
	dataDir, err := os.MkdirTemp("", "cellar-run-check-*")
	if err != nil {
		return fmt.Errorf("create check directory: %w", err)
	}
	defer os.RemoveAll(dataDir)

	server, err := newCheckServer(dataDir)
	if err != nil {
		return err
	}
	defer server.close()
	api := newClient(server.url)

	switch name {
	case "formula-approval":
		err = formulaApproval(api)
	case "vessel-run-lifecycle":
		err = vesselRunLifecycle(api)
	case "observations-completion":
		err = observationsCompletion(api)
	case "rate-limiting":
		err = rateLimiting(dataDir)
	default:
		return fmt.Errorf("unknown workflow check %q", name)
	}
	if err != nil {
		return err
	}
	fmt.Printf("workflow check passed: %s\n", name)
	return nil
}

type checkServer struct {
	url     string
	server  *httptest.Server
	limiter *ratelimit.Limiter
}

func newCheckServer(dataDir string) (*checkServer, error) {
	store, err := storage.Open(dataDir)
	if err != nil {
		return nil, err
	}
	app := service.New(store, clock.System{})
	limiter, err := ratelimit.NewFor(dataDir, clock.System{})
	if err != nil {
		return nil, err
	}
	server := httptest.NewServer(httpapi.NewHandler(app, limiter))
	return &checkServer{url: server.URL, server: server, limiter: limiter}, nil
}

func (c *checkServer) close() {
	c.server.Close()
	c.limiter.Close()
}
