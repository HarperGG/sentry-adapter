package worker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"sentry-adapter/internal/agentos"
	"sentry-adapter/internal/dingtalk"
	"sentry-adapter/internal/store"
)

type analyzer interface {
	Analyze(context.Context, string, string, string) (json.RawMessage, error)
}

type robotSender interface {
	Send(context.Context, dingtalk.Report) error
}

type RCAWorker struct {
	AllowedProjects []string
	Store           *store.Store
	Agent           analyzer
	Robot           robotSender
	Logger          *slog.Logger
}

func (w *RCAWorker) Run(ctx context.Context) {
	poll := time.NewTicker(time.Second)
	defer poll.Stop()
	recoverAt := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-poll.C:
			if !now.Before(recoverAt) {
				if err := w.Store.RecoverExpiredRCA(ctx); err != nil {
					w.Logger.Error("recover feedback analysis jobs", "error", err)
				}
				recoverAt = now.Add(time.Minute)
			}
			j, ok, err := w.Store.ClaimRCA(ctx, agentos.AnalysisTimeout+3*time.Minute, w.AllowedProjects...)
			if err != nil {
				w.Logger.Error("claim feedback analysis job", "error", err)
				continue
			}
			if ok {
				if err := w.process(ctx, j); err != nil {
					w.Logger.Error("update feedback analysis job", "issue_id", j.IssueID, "stage", j.Stage, "error", err)
				}
			}
		}
	}
}

func (w *RCAWorker) process(ctx context.Context, j store.RCAJob) error {
	if j.Stage == store.RCAAnalyzing {
		raw, err := w.Agent.Analyze(ctx, j.IssueID, j.IssueURL, j.EventID)
		finalCtx, finish := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer finish()
		if err != nil {
			var apiErr *agentos.APIError
			if errors.As(err, &apiErr) && !apiErr.Retryable() {
				return w.Store.ReviewRCA(finalCtx, j, apiErr.Error())
			}
			return w.Store.RetryRCA(finalCtx, j, "AgentOS analysis unavailable or incomplete")
		}
		report, err := agentos.ParseReport(raw)
		if err != nil {
			return w.Store.RetryRCA(finalCtx, j, "invalid AgentOS report")
		}
		if !report.Notifiable() {
			return w.Store.ReviewRCAReport(finalCtx, j, raw, "analysis did not yield a supported conclusion")
		}
		return w.Store.SaveRCAReport(finalCtx, j, raw)
	}

	report, err := agentos.ParseReport(j.Report)
	if err != nil || !report.Notifiable() {
		return w.Store.ReviewRCA(ctx, j, "stored report is not deliverable")
	}
	message := dingtalk.Report{
		IssueID: j.IssueID, SentryURL: j.IssueURL, Problem: report.FeedbackSummary.Description,
		Cause: report.RootCause.Conclusion, Category: report.RootCause.Category,
		ConfidenceScore: report.RootCause.ConfidenceScore,
	}
	if steps := report.Recommendations.VerificationSteps; len(steps) > 0 {
		message.NextStep = steps[0]
	}
	sendCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	err = w.Robot.Send(sendCtx, message)
	cancel()
	finalCtx, finish := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer finish()
	if err == nil {
		if err := w.Store.MarkRCADelivered(finalCtx, j); err != nil {
			return err
		}
		w.Logger.Info("feedback analysis delivered", "project", j.Project, "issue_id", j.IssueID)
		return nil
	}
	var rejected *dingtalk.Rejected
	if errors.As(err, &rejected) {
		if !rejected.Retryable() {
			return w.Store.ReviewRCA(finalCtx, j, rejected.Error())
		}
		return w.Store.RetryRCA(finalCtx, j, rejected.Error())
	}
	return w.Store.UncertainRCA(finalCtx, j, "DingTalk delivery outcome unknown; verify in the group")
}
