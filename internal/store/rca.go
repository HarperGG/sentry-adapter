package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	RCAAnalyzing   = "analyzing"
	RCADelivering  = "delivering"
	maxRCAAttempts = 5
)

type RCAJob struct {
	ID                int64
	Organization      string
	Project           string
	IssueID           string
	EventID           string
	AssociatedErrorID string
	IssueURL          string
	FeedbackType      string
	TaskType          string
	DatasetID         string
	Stage             string
	Attempts          int
	Report            json.RawMessage
}

func (s *Store) migrateRCA(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS rca_jobs (
  id bigserial PRIMARY KEY,
  organization text NOT NULL,
  project text NOT NULL,
  issue_id text NOT NULL,
  event_id text NOT NULL DEFAULT '',
  issue_url text NOT NULL,
  status text NOT NULL DEFAULT 'pending_analysis'
    CHECK (status IN ('pending_analysis', 'analyzing', 'pending_delivery',
                      'delivering', 'done', 'needs_review', 'uncertain')),
  analysis_attempts integer NOT NULL DEFAULT 0,
  delivery_attempts integer NOT NULL DEFAULT 0,
  next_attempt_at timestamptz NOT NULL DEFAULT now(),
  lease_until timestamptz,
  report jsonb CHECK (jsonb_typeof(report) = 'object'),
  last_error text,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (organization, project, issue_id)
);
-- All jobs written before feedback_type existed were platform bugs.
ALTER TABLE rca_jobs
  ADD COLUMN IF NOT EXISTS feedback_type text NOT NULL DEFAULT 'platform_bug'
    CHECK (feedback_type IN ('platform_bug','data_issue','other')),
  ADD COLUMN IF NOT EXISTS task_type text NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS dataset_id text NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS associated_error_id text NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS rca_jobs_ready_idx ON rca_jobs (next_attempt_at, id)
  WHERE status IN ('pending_analysis', 'pending_delivery');
CREATE INDEX IF NOT EXISTS rca_jobs_lease_idx ON rca_jobs (lease_until)
  WHERE status IN ('analyzing', 'delivering');`)
	return err
}

// EnqueueRCA can be repeated if the feedback worker is interrupted before creating the card.
func (s *Store) EnqueueRCA(ctx context.Context, j RCAJob) error {
	if j.Organization == "" || j.Project == "" || j.IssueID == "" || j.IssueURL == "" {
		return errors.New("missing RCA feedback identity or URL")
	}
	_, err := s.pool.Exec(ctx, `
INSERT INTO rca_jobs (organization, project, issue_id, event_id, associated_error_id, issue_url, feedback_type, task_type, dataset_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (organization, project, issue_id) DO NOTHING`,
		j.Organization, j.Project, j.IssueID, j.EventID, j.AssociatedErrorID, j.IssueURL, j.FeedbackType, j.TaskType, j.DatasetID)
	return err
}

// ClaimRCA applies the current project policy to both stages, including queued reports.
// An empty allowlist allows every job. The attempt number fences updates from older workers.
func (s *Store) ClaimRCA(ctx context.Context, lease time.Duration, allowedProjects ...string) (RCAJob, bool, error) {
	if lease <= 0 {
		return RCAJob{}, false, errors.New("RCA lease must be positive")
	}
	var j RCAJob
	err := s.pool.QueryRow(ctx, `
WITH next_job AS (
  SELECT id FROM rca_jobs
  WHERE status IN ('pending_analysis', 'pending_delivery') AND next_attempt_at <= now()
    AND (COALESCE(cardinality($2::text[]), 0) = 0 OR project = ANY($2::text[]))
  ORDER BY (status = 'pending_delivery') DESC, next_attempt_at, id FOR UPDATE SKIP LOCKED LIMIT 1
)
UPDATE rca_jobs j SET
  status = CASE j.status WHEN 'pending_analysis' THEN 'analyzing' ELSE 'delivering' END,
  analysis_attempts = j.analysis_attempts + CASE j.status WHEN 'pending_analysis' THEN 1 ELSE 0 END,
  delivery_attempts = j.delivery_attempts + CASE j.status WHEN 'pending_delivery' THEN 1 ELSE 0 END,
  lease_until = now() + $1::double precision * interval '1 second', updated_at = now()
FROM next_job WHERE j.id = next_job.id
RETURNING j.id, j.organization, j.project, j.issue_id, j.event_id, j.associated_error_id, j.issue_url, j.feedback_type, j.task_type, j.dataset_id, j.status,
  CASE j.status WHEN 'analyzing' THEN j.analysis_attempts ELSE j.delivery_attempts END, j.report`,
		lease.Seconds(), allowedProjects).Scan(&j.ID, &j.Organization, &j.Project, &j.IssueID, &j.EventID, &j.AssociatedErrorID,
		&j.IssueURL, &j.FeedbackType, &j.TaskType, &j.DatasetID, &j.Stage, &j.Attempts, &j.Report)
	if errors.Is(err, pgx.ErrNoRows) {
		return RCAJob{}, false, nil
	}
	return j, err == nil, err
}

// SaveRCAReport commits the canonical JSON and schedules notification in one update.
func (s *Store) SaveRCAReport(ctx context.Context, j RCAJob, report json.RawMessage) error {
	return s.finishAnalysis(ctx, j, report, "pending_delivery", "")
}

// ReviewRCAReport retains the model's evidence and outcome without scheduling a delivery.
func (s *Store) ReviewRCAReport(ctx context.Context, j RCAJob, report json.RawMessage, reason string) error {
	return s.finishAnalysis(ctx, j, report, "needs_review", reason)
}

func (s *Store) finishAnalysis(ctx context.Context, j RCAJob, report json.RawMessage, next, reason string) error {
	if j.Stage != RCAAnalyzing || !json.Valid(report) {
		return errors.New("saving an RCA report requires a claimed analysis and valid JSON")
	}
	return s.finishRCA(ctx, j, next, reason, report, 0)
}

func (s *Store) MarkRCADelivered(ctx context.Context, j RCAJob) error {
	if j.Stage != RCADelivering {
		return errors.New("completing a notification requires a claimed delivery")
	}
	return s.finishRCA(ctx, j, "done", "", nil, 0)
}

// RetryRCA is safe for analysis or a notification known to have been rejected.
func (s *Store) RetryRCA(ctx context.Context, j RCAJob, reason string) error {
	if j.Attempts >= maxRCAAttempts {
		return s.ReviewRCA(ctx, j, reason)
	}
	next := "pending_analysis"
	if j.Stage == RCADelivering {
		next = "pending_delivery"
	}
	backoff := time.Second << min(j.Attempts, 6)
	return s.finishRCA(ctx, j, next, reason, nil, backoff)
}

func (s *Store) ReviewRCA(ctx context.Context, j RCAJob, reason string) error {
	return s.finishRCA(ctx, j, "needs_review", reason, nil, 0)
}

func (s *Store) UncertainRCA(ctx context.Context, j RCAJob, reason string) error {
	if j.Stage != RCADelivering {
		return errors.New("ambiguous notification requires a claimed delivery")
	}
	return s.finishRCA(ctx, j, "uncertain", reason, nil, 0)
}

func (s *Store) finishRCA(ctx context.Context, j RCAJob, next, reason string, report json.RawMessage, delay time.Duration) error {
	if j.Stage != RCAAnalyzing && j.Stage != RCADelivering {
		return errors.New("RCA job has no claimed stage")
	}
	tag, err := s.pool.Exec(ctx, `
UPDATE rca_jobs SET status = $4, report = COALESCE($5::jsonb, report), last_error = NULLIF($6, ''),
  lease_until = NULL, next_attempt_at = now() + $7::double precision * interval '1 second', updated_at = now()
WHERE id = $1 AND status = $2 AND lease_until > clock_timestamp() AND
  (CASE status WHEN 'analyzing' THEN analysis_attempts ELSE delivery_attempts END) = $3`,
		j.ID, j.Stage, j.Attempts, next, report, reason, delay.Seconds())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("RCA job %d attempt %d lost its %s claim", j.ID, j.Attempts, j.Stage)
	}
	return nil
}

// An interrupted delivery may already have reached the group. Leave it for reconciliation.
func (s *Store) RecoverExpiredRCA(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
UPDATE rca_jobs SET status = CASE
    WHEN status = 'delivering' THEN 'uncertain'
    WHEN analysis_attempts >= $1 THEN 'needs_review'
    ELSE 'pending_analysis' END,
  last_error = CASE status WHEN 'delivering' THEN 'notification lease expired; delivery unknown'
                           ELSE 'analysis lease expired' END,
  lease_until = NULL, next_attempt_at = now(), updated_at = now()
WHERE status IN ('analyzing','delivering') AND lease_until <= now()`, maxRCAAttempts)
	return err
}
