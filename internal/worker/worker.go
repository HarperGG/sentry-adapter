package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"sentry-adapter/internal/sentry"
	"sentry-adapter/internal/store"
	"sentry-adapter/internal/teambition"
)

type Worker struct {
	Store        *store.Store
	Sentry       *sentry.Client
	Teambition   teambition.Creator
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
	kind := classify(f.Kind)
	if kind == "" {
		w.finish(ctx, log, w.Store.MarkReview(ctx, j.ID, "processing", "feedback type is not mapped"))
		return
	}
	card := cardFromFeedback(f, kind)
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

func classify(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "platform_bug", "feature_gap", "bug", "defect":
		return "bug"
	case "suggestion", "other", "feature", "request":
		return "feature"
	default:
		return ""
	}
}

func title(issueTitle, feedbackType string) string {
	label := feedbackTypeLabel(feedbackType)
	prefix := fmt.Sprintf("【Sentry】【%s】", label)
	value := strings.Join(strings.Fields(issueTitle), " ")
	if value == "" || strings.EqualFold(value, "User Feedback") ||
		strings.EqualFold(value, "Feedback") || value == "用户反馈" {
		return prefix
	}
	// Keep the complete title within the same 100-rune budget used previously.
	remaining := 100 - len([]rune(prefix)) - 1
	runes := []rune(value)
	if len(runes) > remaining {
		value = string(runes[:remaining-1]) + "…"
	}
	return prefix + " " + value
}

func feedbackTypeLabel(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "platform_bug":
		return "平台缺陷"
	case "feature_gap":
		return "功能缺失"
	case "suggestion":
		return "建议"
	case "other":
		return "其他"
	case "bug", "defect":
		return "缺陷"
	case "feature", "request":
		return "需求"
	default:
		return "用户反馈"
	}
}

func cardFromFeedback(f sentry.Feedback, kind string) teambition.Card {
	return teambition.Card{
		SourceIssueID: f.IssueID,
		Title:         title(f.Title, f.Kind),
		Description:   strings.TrimSpace(f.IssueURL),
		Kind:          kind,
	}
}
