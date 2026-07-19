package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"pingachock/internal/api"
	agentapi "pingachock/internal/api/agent"
	publicapi "pingachock/internal/api/public"
	"pingachock/internal/auth"
	"pingachock/internal/store"
	"pingachock/internal/sweeper"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	dsn := getenv("DATABASE_URL", "postgres://pingachock:pingachock@localhost:5433/pingachock?sslmode=disable")
	addr := getenv("LISTEN_ADDR", ":8080")
	migrationsDir := getenv("MIGRATIONS_DIR", "./migrations")
	adminToken := os.Getenv("ADMIN_TOKEN")
	onlineThreshold := time.Duration(getenvInt("NODE_ONLINE_THRESHOLD_SECONDS", 90)) * time.Second
	pollBatchLimit := getenvInt("POLL_BATCH_LIMIT", 50)
	sweepInterval := time.Duration(getenvInt("SWEEP_INTERVAL_SECONDS", 30)) * time.Second
	sweepGrace := time.Duration(getenvInt("SWEEP_GRACE_SECONDS", 600)) * time.Second

	if adminToken == "" {
		log.Warn("ADMIN_TOKEN not set - POST /api/v1/nodes will reject every request")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, dsn)
	if err != nil {
		log.Error("open store", "error", err)
		os.Exit(1)
	}
	defer st.Close()

	if err := st.RunMigrations(ctx, migrationsDir); err != nil {
		log.Error("run migrations", "error", err)
		os.Exit(1)
	}
	log.Info("migrations applied")

	sw := sweeper.New(st, sweepInterval, sweepGrace, log)
	go sw.Run(ctx)

	publicH := publicapi.New(st, onlineThreshold)
	agentH := agentapi.New(st, pollBatchLimit)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	r.Get("/docs", api.ServeDocsUI)
	r.Get("/docs/openapi.yaml", api.ServeOpenAPISpec)

	r.Route("/api/v1", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireAPIKey(st))
			r.Post("/checks", publicH.CreateCheck)
			r.Get("/checks", publicH.ListChecks)
			r.Get("/checks/{id}", publicH.GetCheck)
			r.Delete("/checks/{id}", publicH.CancelCheck)
			r.Get("/nodes", publicH.ListNodes)
			r.Get("/nodes/{id}", publicH.GetNode)
		})
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireAdminToken(adminToken))
			r.Post("/nodes", publicH.CreateNode)
			r.Put("/nodes/{id}", publicH.UpdateNode)
			r.Post("/accounts", publicH.CreateAccount)
			r.Get("/accounts", publicH.ListAccounts)
			r.Post("/accounts/{accountID}/api-keys", publicH.CreateAPIKey)
			r.Get("/accounts/{accountID}/api-keys", publicH.ListAPIKeys)
			r.Delete("/api-keys/{id}", publicH.RevokeAPIKey)
		})
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireNodeSecret(st))
			r.Post("/agent/poll", agentH.Poll)
			r.Post("/agent/results", agentH.Results)
		})
	})

	srv := &http.Server{Addr: addr, Handler: r}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	log.Info("listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "error", err)
		os.Exit(1)
	}
	log.Info("shutdown complete")
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getenvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
