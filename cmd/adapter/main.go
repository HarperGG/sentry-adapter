package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"sentry-adapter/internal/config"
	"sentry-adapter/internal/sentry"
	"sentry-adapter/internal/store"
	"sentry-adapter/internal/teambition"
	"sentry-adapter/internal/webhook"
	"sentry-adapter/internal/worker"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("adapter stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		return err
	}
	if err := db.RecoverExpired(ctx); err != nil {
		return err
	}
	sentryClient, err := sentry.New(cfg.SentryBaseURL, cfg.SentryToken, cfg.SentryAttachmentRedirectHosts...)
	if err != nil {
		return err
	}
	var creator teambition.Creator
	if cfg.TeambitionMode == "mock" {
		creator = teambition.NewMock()
		logger.Warn("mock Teambition mode enabled; no real cards will be created")
	} else {
		creator, err = teambition.New(teambition.Config{
			BaseURL:  cfg.TeambitionBaseURL,
			AuthMode: cfg.TeambitionAuthMode, TokenURL: cfg.TeambitionTokenURL,
			TaskPath: cfg.TeambitionTaskPath, UploadTokenPath: cfg.TeambitionUploadTokenPath,
			AttachmentFieldID:      cfg.TeambitionAttachmentFieldID,
			UploadAllowedOrigins:   cfg.TeambitionUploadAllowedOrigins,
			CustomFieldID:          cfg.TeambitionCustomFieldID,
			CustomFieldOptionID:    cfg.TeambitionCustomFieldOptionID,
			CustomFieldOptionTitle: cfg.TeambitionCustomFieldTitle,
			AppID:                  cfg.TeambitionAppID,
			AppSecret:              cfg.TeambitionAppSecret, TenantID: cfg.TeambitionTenantID,
			OperatorID: cfg.TeambitionOperatorID, ProjectID: cfg.TeambitionProjectID,
			TasklistID: cfg.TeambitionTasklistID, StageID: cfg.TeambitionStageID,
			StatusID: cfg.TeambitionStatusID, ExecutorID: cfg.TeambitionExecutorID,
			BugTypeID: cfg.TeambitionBugTypeID, FeatureTypeID: cfg.TeambitionFeatureTypeID,
		})
		if err != nil {
			return err
		}
		logger.Info("Teambition client ready", "mode", "real", "auth_mode", cfg.TeambitionAuthMode)
	}
	w := &worker.Worker{Store: db, Sentry: sentryClient, Teambition: creator,
		Organization: cfg.SentryOrg, Project: cfg.SentryProject,
		MockMode: cfg.TeambitionMode == "mock", Logger: logger}
	go w.Run(ctx)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(out http.ResponseWriter, _ *http.Request) {
		out.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(out, `{"status":"live"}`)
	})
	mux.HandleFunc("GET /health/ready", func(out http.ResponseWriter, req *http.Request) {
		probe, cancel := context.WithTimeout(req.Context(), 500*time.Millisecond)
		defer cancel()
		if err := db.Ping(probe); err != nil {
			http.Error(out, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		out.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(out, `{"status":"ready"}`)
	})
	mux.HandleFunc("POST /webhooks/sentry/user-feedback", func(out http.ResponseWriter, req *http.Request) {
		if !strings.HasPrefix(req.Header.Get("Content-Type"), "application/json") {
			http.Error(out, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(out, req.Body, webhook.MaxBodyBytes))
		if err != nil {
			http.Error(out, "invalid or oversized body", http.StatusRequestEntityTooLarge)
			return
		}
		delivery, accepted, err := webhook.Parse(body,
			req.Header.Get("Sentry-Hook-Resource"), req.Header.Get("Sentry-Hook-Signature"), cfg.WebhookSecret)
		if err != nil {
			if errors.Is(err, webhook.ErrInvalidSignature) {
				http.Error(out, "invalid signature", http.StatusUnauthorized)
			} else {
				http.Error(out, "invalid webhook", http.StatusBadRequest)
			}
			return
		}
		if !accepted {
			out.WriteHeader(http.StatusNoContent)
			return
		}
		if delivery.Organization != "" && delivery.Organization != cfg.SentryOrg && !allDigits(delivery.Organization) {
			out.WriteHeader(http.StatusNoContent)
			return
		}
		if delivery.Project != "" && delivery.Project != cfg.SentryProject {
			// Sentry may send a numeric project ID. The worker checks the slug
			// from the authenticated Issue API, so do not filter on that here.
			if !allDigits(delivery.Project) {
				out.WriteHeader(http.StatusNoContent)
				return
			}
		}
		deadline, cancel := context.WithTimeout(req.Context(), 750*time.Millisecond)
		defer cancel()
		_, err = db.Enqueue(deadline, store.Job{
			DedupKey: delivery.DedupKey, Organization: cfg.SentryOrg,
			Project: delivery.Project, IssueID: delivery.IssueID, EventID: delivery.EventID,
		})
		if err != nil {
			logger.Error("persist webhook", "error", err)
			http.Error(out, "queue unavailable", http.StatusServiceUnavailable)
			return
		}
		out.Header().Set("Content-Type", "application/json")
		out.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(out, `{"accepted":true}`)
	})
	server := &http.Server{Addr: cfg.ListenAddr, Handler: mux,
		ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second,
		WriteTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second,
		MaxHeaderBytes: 16 << 10}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ListenAndServe() }()
	logger.Info("adapter listening", "addr", cfg.ListenAddr, "project", cfg.SentryProject)
	select {
	case <-ctx.Done():
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return server.Shutdown(shutdownCtx)
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
