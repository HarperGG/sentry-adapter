package worker

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"sentry-adapter/internal/routing"
	"sentry-adapter/internal/sentry"
	"sentry-adapter/internal/store"
	"sentry-adapter/internal/teambition"
)

type Worker struct {
	Store        *store.Store
	Sentry       *sentry.Client
	Teambition   teambition.Creator
	Routing      *routing.Rules
	Organization string
	Project      string
	MockMode     bool
	Logger       *slog.Logger
}

func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	recovery := time.NewTicker(time.Minute)
	defer recovery.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-recovery.C:
			if err := w.Store.RecoverExpired(ctx); err != nil {
				w.Logger.Error("recover jobs", "error", err)
			}
		case <-ticker.C:
			job, ok, err := w.Store.Claim(ctx)
			if err != nil {
				w.Logger.Error("claim job", "error", err)
				continue
			}
			if !ok {
				continue
			}
			jobCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			w.process(jobCtx, job)
			cancel()
		}
	}
}

func (w *Worker) process(ctx context.Context, j store.Job) {
	log := w.Logger.With("job_id", j.ID, "issue_id", j.IssueID)
	if j.Organization != "" && j.Organization != w.Organization {
		w.finish(ctx, log, w.Store.MarkIgnored(ctx, j.ID, "organization not allowed"))
		return
	}
	f, err := w.Sentry.GetFeedback(ctx, w.Organization, j.IssueID, j.EventID)
	if err != nil {
		var apiErr *sentry.APIError
		if errors.As(err, &apiErr) && !apiErr.Retryable() {
			w.finish(ctx, log, w.Store.MarkReview(ctx, j.ID, "processing", err.Error()))
		} else {
			w.finish(ctx, log, w.Store.MarkRetry(ctx, j.ID, "Sentry data temporarily unavailable", j.Attempts))
		}
		return
	}
	if !f.IsFeedback || f.Project != w.Project {
		w.finish(ctx, log, w.Store.MarkIgnored(ctx, j.ID, "not a feedback issue in allowed project"))
		return
	}
	if strings.TrimSpace(f.IssueURL) == "" {
		w.finish(ctx, log, w.Store.MarkReview(ctx, j.ID, "processing", "feedback has no Sentry URL"))
		return
	}
	if strings.TrimSpace(f.Message) == "" {
		w.finish(ctx, log, w.Store.MarkReview(ctx, j.ID, "processing", "feedback message is missing"))
		return
	}
	decision := w.Routing.Resolve(f.Kind, f.TaskType)
	if decision.TagID == "" {
		w.finish(ctx, log, w.Store.MarkReview(ctx, j.ID, "processing", "feedback routing configuration is unavailable"))
		return
	}
	// Missing task metadata must not hide a feedback report. The routing decision
	// leaves its executor empty so the resulting card can be triaged manually.
	if decision.ReviewReason != "" {
		log.Warn("feedback requires manual triage", "reason", decision.ReviewReason)
	}
	card := cardFromFeedback(f, decision)
	card, err = w.Teambition.Prepare(ctx, card)
	if err != nil {
		var permanent *teambition.Permanent
		if errors.As(err, &permanent) {
			w.finish(ctx, log, w.Store.MarkReview(ctx, j.ID, "processing", err.Error()))
		} else {
			w.finish(ctx, log, w.Store.MarkRetry(ctx, j.ID, "Teambition authentication temporarily unavailable", j.Attempts))
		}
		return
	}
	if err := w.Store.MarkCreating(ctx, j.ID); err != nil {
		log.Error("mark creating", "error", err)
		return
	}
	taskID, err := w.Teambition.Create(ctx, card)
	if err != nil {
		var permanent *teambition.Permanent
		if errors.As(err, &permanent) {
			w.finish(ctx, log, w.Store.MarkReview(ctx, j.ID, "creating", err.Error()))
		} else {
			w.finish(ctx, log, w.Store.MarkUncertain(ctx, j.ID, "create request outcome unknown; inspect Teambition before replay"))
		}
		return
	}
	if err := w.Store.MarkDone(ctx, j.ID, taskID); err != nil {
		// A task may exist even if the commit fails. Recovery moves this lease to
		// uncertain instead of sending another create request.
		log.Error("persist created task", "error", err)
		return
	}
	log.Info("card created", "task_id", taskID)
}

func (w *Worker) finish(_ context.Context, log *slog.Logger, err error) {
	if err != nil {
		log.Error("update job", "error", err)
	}
}
