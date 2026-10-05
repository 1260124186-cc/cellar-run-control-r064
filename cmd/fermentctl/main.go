package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/clock"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/config"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/httpapi"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/ratelimit"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/service"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/storage"
	"github.com/1260124186-cc/solo-0016-cellar-run-control/internal/workflowcheck"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "fermentctl:", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	if len(arguments) == 0 {
		return usageError()
	}
	switch arguments[0] {
	case "serve":
		if len(arguments) != 1 {
			return usageError()
		}
		return serve()
	case "check":
		if len(arguments) != 2 {
			return usageError()
		}
		return workflowcheck.Run(arguments[1])
	default:
		return usageError()
	}
}

func usageError() error {
	return errors.New("usage: fermentctl serve | fermentctl check <workflow>")
}

func serve() error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	if err := cfg.EnsureDataDir(); err != nil {
		return err
	}
	store, err := storage.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	app := service.New(store, clock.System{})

	var limiter *ratelimit.Limiter
	if cfg.QuotaEnabled {
		limiter, err = ratelimit.Open(
			filepath.Join(cfg.DataDir, "quota-blocks.json"), cfg.Quota, clock.System{})
		if err != nil {
			return err
		}
	}

	server := &http.Server{
		Addr:              cfg.Address,
		Handler:           httpapi.NewHandler(app, limiter),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for received := range signals {
			if received == syscall.SIGHUP {
				if limiter == nil {
					continue
				}
				// Swap quota rules from the configured file atomically; an
				// invalid file leaves the running rules untouched.
				reloaded, source, reloadErr := config.ReloadQuota()
				if reloadErr != nil {
					log.Printf("quota reload ignored: %v", reloadErr)
					continue
				}
				if reloadErr := limiter.UpdateConfig(reloaded); reloadErr != nil {
					log.Printf("quota reload from %s rejected: %v", source, reloadErr)
					continue
				}
				log.Printf("quota configuration reloaded from %s", source)
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
			if shutdownErr := server.Shutdown(ctx); shutdownErr != nil {
				log.Printf("graceful shutdown failed: %v", shutdownErr)
			}
			cancel()
			return
		}
	}()

	log.Printf("cellar run control listening on %s with data %s", cfg.Address, cfg.DataDir)
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("serve HTTP: %w", err)
}

func init() {
	log.SetFlags(log.LstdFlags | log.LUTC)
}
