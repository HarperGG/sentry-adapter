package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Use a separate schema for each test so no existing feedback jobs can be claimed.
func newRCAStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("RCA_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set RCA_TEST_DATABASE_URL to run PostgreSQL job-state tests")
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgxpool.NewWithConfig(t.Context(), config.Copy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := fmt.Sprintf("rca_test_%d_%d", os.Getpid(), time.Now().UnixNano())
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	s := &Store{pool: pool}
	t.Cleanup(s.Close)
	if err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return s
}

func enqueueRCA(t *testing.T, s *Store, issue string) {
	t.Helper()
	j := RCAJob{Organization: "test", Project: "web", IssueID: issue, IssueURL: "https://example.test/issues/" + issue, FeedbackType: "platform_bug"}
	if err := s.EnqueueRCA(t.Context(), j); err != nil {
		t.Fatal(err)
	}
}

func claimStage(t *testing.T, s *Store, stage string, attempt int, projects ...string) RCAJob {
	t.Helper()
	j, ok, err := s.ClaimRCA(t.Context(), time.Minute, projects...)
	if err != nil || !ok || j.Stage != stage || j.Attempts != attempt {
		t.Fatalf("claim = (%+v, %v, %v), want %s attempt %d", j, ok, err, stage, attempt)
	}
	return j
}

func runRCAQuery(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()
	if _, err := s.pool.Exec(t.Context(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func assertNoRCAClaim(t *testing.T, s *Store, projects ...string) {
	t.Helper()
	if j, ok, err := s.ClaimRCA(t.Context(), time.Minute, projects...); ok || err != nil {
		t.Fatalf("unexpected claim: (%+v, %v, %v)", j, ok, err)
	}
}

func assertRCAStatus(t *testing.T, s *Store, id int64, want string) {
	t.Helper()
	var got string
	if err := s.pool.QueryRow(t.Context(), "SELECT status FROM rca_jobs WHERE id=$1", id).Scan(&got); err != nil || got != want {
		t.Fatalf("job %d status=%q err=%v; want %q", id, got, err, want)
	}
}

func TestRCAMigrationPreservesQueuedPlatformJobs(t *testing.T) {
	s := newRCAStore(t)
	// Rebuild the previous schema in this test's isolated namespace, with a
	// job waiting in each stage at the moment of the upgrade.
	runRCAQuery(t, s, "ALTER TABLE rca_jobs DROP COLUMN feedback_type, DROP COLUMN task_type, DROP COLUMN dataset_id, DROP COLUMN associated_error_id")
	runRCAQuery(t, s, `INSERT INTO rca_jobs (organization, project, issue_id, issue_url, status, report)
		VALUES ('org','web','1','https://example.test/issues/1','pending_analysis',null),
		       ('org','web','2','https://example.test/issues/2','pending_delivery','{"issue_key":"old-report"}')`)
	if err := s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	delivery := claimStage(t, s, RCADelivering, 1)
	analysis := claimStage(t, s, RCAAnalyzing, 1)
	if delivery.IssueID != "2" || analysis.IssueID != "1" || len(delivery.Report) == 0 {
		t.Fatalf("upgrade lost legacy jobs: analysis=%+v delivery=%+v", analysis, delivery)
	}
	for _, j := range []RCAJob{analysis, delivery} {
		if j.FeedbackType != "platform_bug" || j.TaskType != "" || j.DatasetID != "" || j.AssociatedErrorID != "" {
			t.Fatalf("unexpected metadata after upgrade: %+v", j)
		}
	}
}

func TestRCAProjectSwitchPausesQueuedAnalysisAndDelivery(t *testing.T) {
	s := newRCAStore(t)
	ctx := t.Context()
	const testProject = "anno-seg-test"
	const productionProject = "anno-dev"
	enqueue := func(id, project string) {
		t.Helper()
		job := RCAJob{Organization: "test", Project: project, IssueID: id, IssueURL: "https://example.test/issues/" + id, FeedbackType: "platform_bug"}
		if err := s.EnqueueRCA(ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	enqueue("first-prod", productionProject)
	prod := claimStage(t, s, RCAAnalyzing, 1)
	if err := s.SaveRCAReport(ctx, prod, json.RawMessage(`{"issue_key":"first-prod"}`)); err != nil {
		t.Fatal(err)
	}
	enqueue("second-prod", productionProject)
	enqueue("similar-name", "anno-seg-test-2")
	enqueue("test-only", testProject)
	testJob := claimStage(t, s, RCAAnalyzing, 1, testProject)
	if testJob.IssueID != "test-only" || testJob.Project != testProject {
		t.Fatalf("unexpected job for testing rollout: %+v", testJob)
	}
	assertNoRCAClaim(t, s, testProject)

	// Widening the rollout resumes both original stages with their first attempt.
	resumedDelivery := claimStage(t, s, RCADelivering, 1, testProject, productionProject)
	resumedAnalysis := claimStage(t, s, RCAAnalyzing, 1, testProject, productionProject)
	if resumedDelivery.ID != prod.ID || resumedAnalysis.IssueID != "second-prod" {
		t.Fatalf("wrong production jobs resumed: delivery %s and analysis %s", resumedDelivery.IssueID, resumedAnalysis.IssueID)
	}
	assertNoRCAClaim(t, s, testProject, productionProject)
	unrestricted := claimStage(t, s, RCAAnalyzing, 1)
	if unrestricted.IssueID != "similar-name" {
		t.Fatalf("unexpected job after lifting filter: %+v", unrestricted)
	}
}

func TestRCAStoredReportSurvivesDeliveryRetry(t *testing.T) {
	s := newRCAStore(t)
	ctx := t.Context()
	job := RCAJob{Organization: "test", Project: "web", IssueID: "42", IssueURL: "https://example.test/issues/42",
		FeedbackType: "data_issue", TaskType: "Driving_Lane", DatasetID: "573124",
		EventID: "f0a00001000000000000000000000001", AssociatedErrorID: "20a10002000000000000000000000002"}
	if err := s.EnqueueRCA(ctx, job); err != nil {
		t.Fatal(err)
	}
	// Duplicate webhooks cannot change an in-flight report's analysis mode or task identity.
	enqueueRCA(t, s, "42")
	analysis := claimStage(t, s, RCAAnalyzing, 1)
	assertNoRCAClaim(t, s)
	report := json.RawMessage(`{"issue_key":"42"}`)
	if err := s.SaveRCAReport(ctx, analysis, report); err != nil {
		t.Fatal(err)
	}
	delivery := claimStage(t, s, RCADelivering, 1)
	if err := s.RetryRCA(ctx, delivery, "rejected before delivery"); err != nil {
		t.Fatal(err)
	}
	assertNoRCAClaim(t, s)
	runRCAQuery(t, s, "UPDATE rca_jobs SET next_attempt_at=now() WHERE id=$1", delivery.ID)
	next := claimStage(t, s, RCADelivering, 2)
	for _, claimed := range []RCAJob{analysis, delivery, next} {
		if claimed.FeedbackType != job.FeedbackType || claimed.TaskType != job.TaskType || claimed.DatasetID != job.DatasetID ||
			claimed.EventID != job.EventID || claimed.AssociatedErrorID != job.AssociatedErrorID {
			t.Fatalf("claim lost original feedback metadata: %+v", claimed)
		}
	}
	var restored struct {
		Issue string `json:"issue_key"`
	}
	if err := json.Unmarshal(next.Report, &restored); err != nil || restored.Issue != "42" || next.IssueURL != "https://example.test/issues/42" {
		t.Fatalf("stored report=%q issue_url=%q, err=%v", next.Report, next.IssueURL, err)
	}
	if s.MarkRCADelivered(ctx, delivery) == nil {
		t.Fatal("stale delivery claim accepted")
	}
	if err := s.MarkRCADelivered(ctx, next); err != nil {
		t.Fatal(err)
	}
	enqueueRCA(t, s, "42")
	assertRCAStatus(t, s, analysis.ID, "done")
	assertNoRCAClaim(t, s)
}

func TestRCARecoveryFencesClaimsAndStopsUnknownDelivery(t *testing.T) {
	s := newRCAStore(t)
	ctx := t.Context()
	enqueueRCA(t, s, "43")
	old := claimStage(t, s, RCAAnalyzing, 1)
	expire := "UPDATE rca_jobs SET lease_until=now()-interval '1 second' WHERE id=$1"
	runRCAQuery(t, s, expire, old.ID)
	report := json.RawMessage(`{"issue_key":"43"}`)
	if s.SaveRCAReport(ctx, old, report) == nil {
		t.Fatal("expired analysis accepted before recovery")
	}
	if err := s.RecoverExpiredRCA(ctx); err != nil {
		t.Fatal(err)
	}
	current := claimStage(t, s, RCAAnalyzing, 2)
	if s.RetryRCA(ctx, old, "stale error") == nil {
		t.Fatal("stale analysis accepted after recovery")
	}
	if err := s.SaveRCAReport(ctx, current, report); err != nil {
		t.Fatal(err)
	}
	delivery := claimStage(t, s, RCADelivering, 1)
	runRCAQuery(t, s, expire, delivery.ID)
	if err := s.RecoverExpiredRCA(ctx); err != nil {
		t.Fatal(err)
	}
	assertRCAStatus(t, s, delivery.ID, "uncertain")
	assertNoRCAClaim(t, s)
}

func TestRCAReviewOutcomes(t *testing.T) {
	s := newRCAStore(t)
	ctx := t.Context()
	enqueueRCA(t, s, "44")
	j := claimStage(t, s, RCAAnalyzing, 1)
	if err := s.ReviewRCAReport(ctx, j, json.RawMessage(`{"analysis_status":"信息不足"}`), "no evidence"); err != nil {
		t.Fatal(err)
	}
	var status string
	err := s.pool.QueryRow(ctx, "SELECT report->>'analysis_status' FROM rca_jobs WHERE id=$1", j.ID).Scan(&status)
	if err != nil || status != "信息不足" {
		t.Fatalf("review report status=%q err=%v", status, err)
	}
	assertRCAStatus(t, s, j.ID, "needs_review")
	enqueueRCA(t, s, "45")
	for attempt := 1; attempt <= maxRCAAttempts; attempt++ {
		j = claimStage(t, s, RCAAnalyzing, attempt)
		if err := s.RetryRCA(ctx, j, "temporarily unavailable"); err != nil {
			t.Fatal(err)
		}
		runRCAQuery(t, s, "UPDATE rca_jobs SET next_attempt_at=now() WHERE id=$1", j.ID)
	}
	assertRCAStatus(t, s, j.ID, "needs_review")
	assertNoRCAClaim(t, s)
}
