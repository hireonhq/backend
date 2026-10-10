// Command api is the HTTP entry point.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/joho/godotenv"

	"github.com/hireonai/backend-hireon-go/internal/auth"
	"github.com/hireonai/backend-hireon-go/internal/config"
	"github.com/hireonai/backend-hireon-go/internal/handler"
	"github.com/hireonai/backend-hireon-go/internal/httpx"
	"github.com/hireonai/backend-hireon-go/internal/repository/postgres"
	"github.com/hireonai/backend-hireon-go/internal/service"
	"github.com/hireonai/backend-hireon-go/internal/storage"
	"github.com/hireonai/backend-hireon-go/internal/worker"
)

// Set at build time via -ldflags -X, so a running container can be identified.
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	_ = godotenv.Load()
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	httpx.SetupLogger(cfg.IsProduction())
	slog.Info("starting hireon backend", "version", version, "commit", commit)

	if err := run(cfg); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(cfg config.Config) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	pool, err := postgres.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	slog.Info("database connected")

	verifier, err := auth.NewVerifier(ctx, cfg.SupabaseURL)
	if err != nil {
		return err
	}
	defer verifier.Close()
	slog.Info("supabase jwks loaded")

	store := postgres.New(pool)
	mlClient := service.NewMLRecommender(cfg)
	objects, err := storage.New(ctx, cfg)
	if err != nil {
		// Object storage is required for uploads; failing fast beats a runtime
		// surprise on the first upload.
		return err
	}

	// Durable work queue: jobs are persisted before the request is acknowledged,
	// so a deploy or crash retries them instead of losing them.
	queue := store.Queue()

	refSvc := service.NewReferenceService(store)
	jobsSvc := service.NewJobsService(store, mlClient)
	profileSvc := service.NewProfileService(store, objects, queue)

	refHandler := handler.NewReference(refSvc)
	jobsHandler := handler.NewJobs(jobsSvc)
	profileHandler := handler.NewProfile(profileSvc, queue)

	r := chi.NewRouter()
	r.Use(httpx.Recover)
	r.Use(httpx.RequestIDMiddleware)
	r.Use(httpx.RequestLogger)

	// Health probes: Cloud Run needs an endpoint that does not touch the database.
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	// Readiness additionally checks the database.
	r.Get("/readyz", func(w http.ResponseWriter, req *http.Request) {
		if err := pool.Ping(req.Context()); err != nil {
			httpx.Error(w, http.StatusServiceUnavailable, "database unavailable")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	})

	r.Get("/", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "/docs", http.StatusFound)
	})

	r.Route("/job-categories", func(r chi.Router) {
		r.Get("/", refHandler.JobCategories)
	})
	r.Route("/job-min-experiences", func(r chi.Router) {
		r.Get("/", refHandler.JobMinExperiences)
	})
	r.Route("/company-industries", func(r chi.Router) {
		r.Get("/", refHandler.CompanyIndustries)
	})

	// /jobs accepts an optional token: a valid one personalises results, an
	// absent one serves the anonymous listing.
	r.Group(func(r chi.Router) {
		r.Use(httpx.OptionalAuth(verifier, store))
		r.Get("/jobs", jobsHandler.List)
	})
	// Job detail and every profile route require a token.
	r.Group(func(r chi.Router) {
		r.Use(httpx.RequireAuth(verifier, store))
		r.Get("/jobs/{jobId}", jobsHandler.Detail)
		r.Get("/profile", profileHandler.Get)
		r.Put("/profile", profileHandler.Update)
		r.Patch("/profile/tag-preferences", profileHandler.UpdateTagPreferences)
		r.Post("/profile/bookmark-job", profileHandler.AddBookmark)
		r.Delete("/profile/bookmark-job/{jobId}", profileHandler.DeleteBookmark)
		r.Patch("/profile/photo", profileHandler.UploadPhoto)
		r.Patch("/profile/cv", profileHandler.UploadCV)
		r.Delete("/profile/cv", profileHandler.DeleteCV)
		// Background work is asynchronous, so clients need a way to observe it.
		r.Get("/profile/jobs/{jobId}", profileHandler.JobStatus)
	})

	// Background worker pool. It shares the process but not the request lifetime:
	// work is claimed from the database, so it survives a restart and two replicas
	// never process the same row.
	workers := worker.New(queue, []worker.Handler{
		service.NewCVEmbeddingHandler(store, mlClient),
	}, worker.Options{Concurrency: 2, MaxAttempts: 3})
	workers.Start(ctx)
	defer workers.Stop()

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Generous write timeout: analysis and cover-letter calls may stream.
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  2 * time.Minute,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("server listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("shutdown signal received")
	}

	// Drain in-flight requests before exiting.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	return srv.Shutdown(shutdownCtx)
}
