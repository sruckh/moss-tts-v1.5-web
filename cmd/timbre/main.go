// Command timbre serves the Timbre TTS web app.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/sruckh/timbre/internal/auth"
	"github.com/sruckh/timbre/internal/config"
	"github.com/sruckh/timbre/internal/db"
	"github.com/sruckh/timbre/internal/jobs"
	"github.com/sruckh/timbre/internal/runpod"
	"github.com/sruckh/timbre/internal/server"
	"github.com/sruckh/timbre/internal/voices"
	"github.com/sruckh/timbre/internal/worker"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Never log the key itself — only whether Infisical injected one.
	log.Info("config loaded",
		"addr", cfg.Addr,
		"db", cfg.DBPath,
		"runpod_endpoint", cfg.RunPodEndpoint,
		"runpod_key_present", cfg.HasRunPodKey())

	if err := os.MkdirAll(cfg.AudioDir, 0o750); err != nil {
		return err
	}

	handle, err := db.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer handle.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := db.Migrate(ctx, handle); err != nil {
		return err
	}

	authManager, ephemeralKey, err := auth.NewManager(handle,
		[]byte(cfg.SessionSecret), cfg.SecureCookies())
	if err != nil {
		return err
	}
	if ephemeralKey {
		log.Warn("TIMBRE_SESSION_SECRET unset; generated an ephemeral key — " +
			"sessions will not survive restarts")
	}
	if err := authManager.Bootstrap(ctx, log, cfg.AdminUsername, cfg.AdminPassword); err != nil {
		return err
	}

	voiceStore := voices.NewStore(handle, cfg.AudioDir)
	if err := voiceStore.SeedStock(ctx); err != nil {
		return err
	}
	log.Info("voice library ready", "stock_seed", "done")

	jobStore := jobs.NewStore(handle)
	runpodClient := runpod.New(cfg.RunPodEndpoint, cfg.RunPodAPIKey)

	// The submission worker is the only caller of RunPod. It starts even when
	// the endpoint or key is missing: queued jobs then fail with a recorded
	// reason, which is far easier to diagnose than a queue that never moves.
	if !runpodClient.Configured() {
		log.Warn("RunPod endpoint or API key missing; queued jobs will fail until both are set")
	}
	submitter := worker.New(jobStore, voiceStore, runpodClient, cfg.MaxInFlight, log)

	var workerDone sync.WaitGroup
	workerDone.Add(1)
	go func() {
		defer workerDone.Done()
		submitter.Run(ctx)
	}()

	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           server.New(cfg, handle, authManager, voiceStore, jobStore, runpodClient),
		ReadHeaderTimeout: 10 * time.Second,
		// Generous but finite: every browser-facing request is sub-second,
		// and uploads of reference audio are the only large bodies.
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	errs := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr)
		if err := httpServer.ListenAndServe(); err != nil &&
			!errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err = httpServer.Shutdown(shutdownCtx)

	// ctx is already cancelled here, so the worker is on its way out; wait so a
	// submission in flight finishes recording its outcome before the DB closes.
	stop()
	workerDone.Wait()
	return err
}
