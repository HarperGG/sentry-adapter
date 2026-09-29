package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Job struct {
	ID           int64
	DedupKey     string
	Organization string
	Project      string
	IssueID      string
	EventID      string
	Attempts     int
}

type Store struct{ pool *pgxpool.Pool }

func Open(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close()                         { s.pool.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

func (s *Store) Migrate(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS feedback_jobs (
  id bigserial PRIMARY KEY,
  dedup_key text NOT NULL UNIQUE,
  organization text NOT NULL,
  project text NOT NULL DEFAULT '',
  issue_id text NOT NULL,
  event_id text NOT NULL DEFAULT '',
  status text NOT NULL DEFAULT 'pending',
  attempts integer NOT NULL DEFAULT 0,
  next_attempt_at timestamptz NOT NULL DEFAULT now(),
  lease_until timestamptz,
  task_id text,
  last_error text,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
)`)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `CREATE INDEX IF NOT EXISTS feedback_jobs_ready_idx
  ON feedback_jobs (next_attempt_at, id)
	WHERE status IN ('pending', 'retry')`)
	return err
}

// Enqueue commits the job before the webhook handler acknowledges delivery.
func (s *Store) Enqueue(ctx context.Context, j Job) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
INSERT INTO feedback_jobs (dedup_key, organization, project, issue_id, event_id)
VALUES ($1, $2, $3, $4, $5) ON CONFLICT (dedup_key) DO NOTHING`,
		j.DedupKey, j.Organization, j.Project, j.IssueID, j.EventID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Store) Claim(ctx context.Context) (Job, bool, error) {
	var j Job
	err := s.pool.QueryRow(ctx, `
WITH next_job AS (
  SELECT id FROM feedback_jobs
  WHERE status IN ('pending', 'retry') AND next_attempt_at <= now()
  ORDER BY next_attempt_at, id FOR UPDATE SKIP LOCKED LIMIT 1
)
UPDATE feedback_jobs j
SET status = 'processing', attempts = attempts + 1,
    lease_until = now() + interval '10 minutes', updated_at = now()
FROM next_job WHERE j.id = next_job.id
RETURNING j.id, j.dedup_key, j.organization, j.project, j.issue_id,
          j.event_id, j.attempts`).Scan(&j.ID, &j.DedupKey, &j.Organization,
		&j.Project, &j.IssueID, &j.EventID, &j.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	return j, true, nil
}

func (s *Store) MarkCreating(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `
UPDATE feedback_jobs SET status='creating', lease_until=now() + interval '10 minutes',
  last_error=NULL, updated_at=now()
WHERE id=$1 AND status='processing'`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("job %d transition processing -> creating did not apply", id)
	}
	return nil
}
func (s *Store) MarkDone(ctx context.Context, id int64, taskID string) error {
	if taskID == "" {
		return errors.New("missing Teambition task ID")
	}
	return s.transition(ctx, id, "creating", "done", taskID, "", 0)
}
func (s *Store) MarkIgnored(ctx context.Context, id int64, reason string) error {
	return s.transition(ctx, id, "processing", "ignored", "", reason, 0)
}
func (s *Store) MarkReview(ctx context.Context, id int64, from, reason string) error {
	return s.transition(ctx, id, from, "needs_review", "", reason, 0)
}
func (s *Store) MarkUncertain(ctx context.Context, id int64, reason string) error {
	return s.transition(ctx, id, "creating", "uncertain", "", reason, 0)
}
func (s *Store) MarkRetry(ctx context.Context, id int64, reason string, attempts int) error {
	if attempts >= 5 {
		return s.MarkReview(ctx, id, "processing", reason)
	}
	backoff := time.Duration(1<<min(attempts, 6)) * time.Second
	return s.transition(ctx, id, "processing", "retry", "", reason, backoff)
}

func (s *Store) transition(ctx context.Context, id int64, from, to, taskID, reason string, delay time.Duration) error {
	tag, err := s.pool.Exec(ctx, `
UPDATE feedback_jobs SET status=$3, task_id=COALESCE(NULLIF($4, ''), task_id),
  last_error=NULLIF($5, ''), lease_until=NULL,
  next_attempt_at=now() + ($6::double precision * interval '1 second'), updated_at=now()
WHERE id=$1 AND status=$2`, id, from, to, taskID, reason,
		delay.Seconds())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("job %d transition %s -> %s did not apply", id, from, to)
	}
	return nil
}

func (s *Store) RecoverExpired(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
UPDATE feedback_jobs SET status = CASE WHEN status = 'creating' THEN 'uncertain' ELSE 'retry' END,
  last_error = 'worker lease expired; inspect uncertain Teambition create before replay',
  lease_until = NULL, next_attempt_at = now(), updated_at = now()
WHERE status IN ('processing', 'creating') AND lease_until < now()`)
	return err
}
