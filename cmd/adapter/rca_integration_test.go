package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"sentry-adapter/internal/agentos"
	"sentry-adapter/internal/config"
	"sentry-adapter/internal/dingtalk"
	"sentry-adapter/internal/routing"
	"sentry-adapter/internal/sentry"
	"sentry-adapter/internal/store"
	"sentry-adapter/internal/teambition"
	"sentry-adapter/internal/worker"
)

const (
	integrationOrg             = "sentry"
	integrationCardProject     = "anno-dev"
	integrationAnalysisProject = "anno-seg-test"
	integrationHookKey         = "local-test-signing-key"
	integrationNextStep        = "核对保存请求的错误栈"
)

type integrationIssue struct {
	id, project, projectID, cloud, environment, kind, problem string
	wantCard, wantRCA                                         bool
}

func (i integrationIssue) eventID() string { return strings.Repeat("a", 30) + i.id }

func (i integrationIssue) link(baseURL string) string {
	return baseURL + "/organizations/" + integrationOrg + "/issues/feedback/?feedbackSlug=" +
		url.QueryEscape(i.project+":"+i.id) + "&project=" + i.projectID
}

func TestSignedFeedbackRoutesRCAIndependentlyOfTeambitionProject(t *testing.T) {
	db, sql := openIntegrationStore(t)
	issues := []integrationIssue{
		{id: "42", project: integrationCardProject, projectID: "5", cloud: "TC", environment: "production", kind: "platform_bug", problem: "腾讯云保存失败", wantCard: true},
		{id: "43", project: integrationAnalysisProject, projectID: "8", cloud: "HS", environment: "test", kind: "platform_bug", problem: "火山云保存失败", wantRCA: true},
		{id: "44", project: integrationCardProject, projectID: "5", cloud: "HS", environment: "production", kind: "data_issue", problem: "标注数据缺失", wantCard: true},
		{id: "45", project: integrationAnalysisProject, projectID: "8", cloud: "TC", environment: "test", kind: "suggestion", problem: "增加批量保存入口"},
		{id: "46", project: integrationAnalysisProject, projectID: "8", environment: "production", kind: "platform_bug", problem: "测试项目携带生产标签", wantRCA: true},
		{id: "47", project: integrationCardProject, projectID: "5", kind: "other", problem: "其他反馈", wantCard: true},
		{id: "48", project: integrationCardProject, projectID: "5", cloud: "HS", environment: "test", kind: "platform_bug", problem: "线上项目携带测试标签", wantCard: true},
		{id: "49", project: integrationAnalysisProject, projectID: "8", cloud: "TC", kind: "platform_bug", problem: "测试反馈缺少环境标签", wantRCA: true},
	}
	sentryServer := newIntegrationSentry(t, issues)
	sentryClient, err := sentry.New(sentryServer.URL, "local-test-token")
	if err != nil {
		t.Fatal(err)
	}
	analysisRequests := make(chan map[string]string, 32)
	agentServer, sessionCount := newIntegrationAgent(t, issues, sentryServer.URL, analysisRequests)
	agent, err := agentos.New(agentServer.URL, "test-agent", "test-user", "")
	if err != nil {
		t.Fatal(err)
	}
	cards := make(integrationTeambition, 32)
	messages := make(integrationDingTalk, 32)
	rules, err := routing.Load("../../config/feedback-routing.json")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(t.Output(), nil))
	cfg := config.Config{
		WebhookSecret: integrationHookKey, SentryOrg: integrationOrg, SentryProject: integrationCardProject,
		RCAEnabled: true, RCAAllowedProjects: []string{integrationAnalysisProject},
	}
	handler := feedbackHandler(cfg, db, logger)
	var expectedCards, expectedAnalyses int
	for _, issue := range issues {
		postIntegrationWebhook(t, handler, "issue", integrationIssueDelivery(issue, integrationOrg), integrationHookKey, http.StatusAccepted)
		if issue.wantCard {
			expectedCards++
		}
		if issue.wantRCA {
			expectedAnalyses++
		}
	}
	postIntegrationWebhook(t, handler, "issue", integrationIssueDelivery(issues[1], integrationOrg), integrationHookKey, http.StatusAccepted)
	postIntegrationWebhook(t, handler, "issue", integrationIssueDelivery(issues[1], "another-org"), integrationHookKey, http.StatusNoContent)
	postIntegrationWebhook(t, handler, "issue", integrationIssueDelivery(issues[0], integrationOrg), "wrong-key", http.StatusUnauthorized)
	cfg.RCAEnabled = false
	postIntegrationWebhook(t, feedbackHandler(cfg, db, logger), "issue", integrationIssueDelivery(issues[1], integrationOrg), integrationHookKey, http.StatusNoContent)

	ctx, cancel := context.WithCancel(t.Context())
	var workers sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		workers.Wait()
	})
	feedbackWorker := worker.Worker{
		Store: db, Sentry: sentryClient, Teambition: cards, Routing: rules,
		Organization: integrationOrg, Project: integrationCardProject, RCAEnabled: true,
		RCAAllowedProjects: cfg.RCAAllowedProjects, Logger: logger,
	}
	rcaWorker := worker.RCAWorker{Store: db, Agent: agent, Robot: messages, AllowedProjects: cfg.RCAAllowedProjects, Logger: logger}
	workers.Go(func() { feedbackWorker.Run(ctx) })
	workers.Go(func() { rcaWorker.Run(ctx) })
	waitIntegrationJobs(t, sql, len(issues), expectedAnalyses)

	// The alternate webhook source delivers the same issue after both workers finish.
	second := issues[1]
	alert := fmt.Sprintf(`{"action":"triggered","organization":{"slug":%q},"data":{"event":{"issue_id":%q,"event_id":%q,"issueCategory":"feedback","project":{"slug":%q}}}}`,
		integrationOrg, second.id, second.eventID(), second.project)
	postIntegrationWebhook(t, handler, "event_alert", alert, integrationHookKey, http.StatusAccepted)
	cancel()
	workers.Wait()

	var feedbackRows, rcaRows int
	err = sql.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM feedback_jobs), (SELECT count(*) FROM rca_jobs)`).Scan(&feedbackRows, &rcaRows)
	if err != nil || feedbackRows != len(issues) || rcaRows != expectedAnalyses {
		t.Fatalf("duplicate or excluded webhooks changed persisted jobs: feedback=%d, analysis=%d, error=%v", feedbackRows, rcaRows, err)
	}
	if int(sessionCount.Load()) != expectedAnalyses || len(analysisRequests) != expectedAnalyses || len(cards) != expectedCards || len(messages) != expectedAnalyses {
		t.Fatalf("unexpected side effects: sessions=%d analysis=%d cards=%d messages=%d", sessionCount.Load(), len(analysisRequests), len(cards), len(messages))
	}
	sessions := make(map[string]bool)
	for range expectedAnalyses {
		request := <-analysisRequests
		session := request["sessionId"]
		if session == "" || sessions[session] {
			t.Fatal("feedback analyses did not use separate AgentOS sessions")
		}
		sessions[session] = true
	}
	actualCards := make(map[string]teambition.Card)
	for range expectedCards {
		card := <-cards
		actualCards[card.SourceIssueID] = card
	}
	actualMessages := make(map[string]dingtalk.Report)
	for range expectedAnalyses {
		message := <-messages
		actualMessages[message.IssueID] = message
	}
	for _, issue := range issues {
		assertIntegrationIssue(t, sql, rules, issue, sentryServer.URL, actualCards, actualMessages)
	}
}

func openIntegrationStore(t *testing.T) (*store.Store, *pgxpool.Pool) {
	t.Helper()
	databaseURL := os.Getenv("RCA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set RCA_TEST_DATABASE_URL to run the feedback webhook integration test")
	}
	admin, err := pgxpool.New(t.Context(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := fmt.Sprintf("adapter_rca_test_%d_%d", os.Getpid(), time.Now().UnixNano())
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
	scoped, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	query := scoped.Query()
	query.Set("search_path", schema)
	scoped.RawQuery = query.Encode()
	sql, err := pgxpool.New(t.Context(), scoped.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sql.Close)
	db, err := store.Open(t.Context(), scoped.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return db, sql
}

func integrationIssueDelivery(issue integrationIssue, org string) string {
	return fmt.Sprintf(`{"action":"created","organization":{"slug":%q},"data":{"issue":{"id":%q,"issueCategory":"FEEDBACK","project":{"slug":%q}}}}`, org, issue.id, issue.project)
}

func postIntegrationWebhook(t *testing.T, handler http.HandlerFunc, resource, body, key string, wantStatus int) {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = io.WriteString(mac, body)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/sentry/user-feedback", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sentry-Hook-Resource", resource)
	req.Header.Set("Sentry-Hook-Signature", hex.EncodeToString(mac.Sum(nil)))
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, req)
	if out.Code != wantStatus {
		t.Fatalf("webhook status = %d, want %d, body = %s", out.Code, wantStatus, out.Body.String())
	}
}

func newIntegrationSentry(t *testing.T, issues []integrationIssue) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, issue := range issues {
			path := "/api/0/organizations/" + integrationOrg + "/issues/" + issue.id + "/"
			switch r.URL.Path {
			case path:
				_, _ = fmt.Fprintf(w, `{"id":%q,"issueCategory":"feedback","project":{"id":%q,"slug":%q}}`,
					issue.id, issue.projectID, issue.project)
				return
			case path + "events/oldest/", path + "events/" + issue.eventID() + "/":
				businessURL := "https://annotation.example/labels"
				if issue.cloud != "" {
					businessURL += "?server=" + issue.cloud
				}
				tags := []map[string]string{{"key": "feedback_type", "value": issue.kind}, {"key": "task_type", "value": "OD"}}
				event := map[string]any{
					"eventID":  issue.eventID(),
					"contexts": map[string]any{"feedback": map[string]string{"message": issue.problem, "url": businessURL}},
				}
				if issue.environment != "" {
					event["environment"] = issue.environment
					tags = append(tags, map[string]string{"key": "environment", "value": issue.environment})
				}
				event["tags"] = tags
				if err := json.NewEncoder(w).Encode(event); err != nil {
					t.Error(err)
				}
				return
			}
		}
		t.Errorf("unexpected Sentry request: %s", r.URL.Path)
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

func newIntegrationAgent(t *testing.T, issues []integrationIssue, sentryURL string, requests chan<- map[string]string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	sessionCount := new(atomic.Int32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("invalid AgentOS request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req["agentId"] != "test-agent" || req["userId"] != "test-user" {
			t.Error("AgentOS request omitted configured app or user ID")
		}
		if r.URL.Path == "/create_session" {
			_, _ = fmt.Fprintf(w, `{"code":"0000","data":{"sessionId":"session-%d"}}`, sessionCount.Add(1))
			return
		}
		if r.URL.Path == "/chat_stream" {
			requests <- req
			for _, issue := range issues {
				link := issue.link(sentryURL)
				if issue.wantRCA && strings.Contains(req["message"], link) && strings.Contains(req["message"], issue.eventID()) {
					report := fmt.Sprintf(`{"analysis_status":"分析完成","issue_key":%q,"feedback_summary":{"description":%q},"root_cause":{"conclusion":"保存接口报错","confidence":"高","confidence_score":75,"category":"平台异常"},"evidence":[{"observation":"保存异常","reference":%q}],"recommendations":{"verification_steps":[%q]},"test_cloud":%q}`,
						issue.id, issue.problem, link, integrationNextStep, issue.cloud)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "data: {\"type\":\"text\",\"content\":%s}\n\nevent:done\ndata:{\"type\":\"done\",\"reason\":\"completed\"}\n\n", jsonString(report))
					return
				}
			}
			t.Error("AgentOS analysis lost the Sentry link or exact event identity, or analyzed a non-platform issue")
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	return server, sessionCount
}

func jsonString(value string) []byte {
	encoded, _ := json.Marshal(value)
	return encoded
}

type integrationTeambition chan teambition.Card

func (sender integrationTeambition) Prepare(_ context.Context, card teambition.Card) (teambition.Card, error) {
	return card, nil
}

func (sender integrationTeambition) Create(_ context.Context, card teambition.Card) (string, error) {
	sender <- card
	return "test-task-" + card.SourceIssueID, nil
}

type integrationDingTalk chan dingtalk.Report

func (sender integrationDingTalk) Send(_ context.Context, report dingtalk.Report) error {
	sender <- report
	return nil
}

func waitIntegrationJobs(t *testing.T, sql *pgxpool.Pool, feedbackCount, deliveryCount int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var feedbackDone, delivered int
	for {
		err := sql.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM feedback_jobs WHERE status IN ('done', 'ignored')),
			(SELECT count(*) FROM rca_jobs WHERE status='done')`).Scan(&feedbackDone, &delivered)
		if err != nil {
			logIntegrationRCAState(t, sql)
			t.Fatalf("poll jobs: finished feedback = %d/%d, DingTalk deliveries = %d/%d: %v", feedbackDone, feedbackCount, delivered, deliveryCount, err)
		}
		if feedbackDone == feedbackCount && delivered == deliveryCount {
			return
		}
		select {
		case <-ctx.Done():
			logIntegrationRCAState(t, sql)
			t.Fatalf("job timeout: finished feedback = %d/%d, DingTalk deliveries = %d/%d", feedbackDone, feedbackCount, delivered, deliveryCount)
		case <-ticker.C:
		}
	}
}

func logIntegrationRCAState(t *testing.T, sql *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	var state string
	if err := sql.QueryRow(ctx, `SELECT coalesce(string_agg(concat(issue_id, ':', status, ':', last_error), ', ' ORDER BY issue_id), '') FROM rca_jobs`).Scan(&state); err == nil {
		t.Logf("RCA job state: %s", state)
	}
}

func assertIntegrationIssue(t *testing.T, sql *pgxpool.Pool, rules *routing.Rules, issue integrationIssue, baseURL string, cards map[string]teambition.Card, messages map[string]dingtalk.Report) {
	t.Helper()
	var status, taskID string
	var attempts int
	if err := sql.QueryRow(t.Context(), `SELECT status, coalesce(task_id, ''), attempts FROM feedback_jobs WHERE issue_id=$1`, issue.id).Scan(&status, &taskID, &attempts); err != nil {
		t.Fatal(err)
	}
	card, cardCreated := cards[issue.id]
	if attempts != 1 || cardCreated != issue.wantCard {
		t.Errorf("Issue %s attempts=%d, card created=%t, want one attempt and card=%t", issue.id, attempts, cardCreated, issue.wantCard)
	}
	if issue.wantCard {
		decision := rules.Resolve(issue.kind, "OD", issue.cloud)
		wantTags := []string{decision.TagID}
		if issue.cloud != "" {
			wantTags = append(wantTags, decision.EnvironmentTagID)
		}
		if status != "done" || taskID != "test-task-"+issue.id || card.FeedbackType != issue.kind || !strings.Contains(card.Description, issue.problem+"\n\n"+issue.link(baseURL)) ||
			!reflect.DeepEqual(card.TagIDs, wantTags) {
			t.Errorf("Issue %s existing card flow changed: status=%q task=%q card=%+v", issue.id, status, taskID, card)
		}
	} else if status != "ignored" || taskID != "" {
		t.Errorf("Issue %s outside card project scope: status=%q task=%q", issue.id, status, taskID)
	}
	message, notified := messages[issue.id]
	if notified != issue.wantRCA {
		t.Errorf("Issue %s notification = %t, want %t", issue.id, notified, issue.wantRCA)
	}
	if !issue.wantRCA {
		var exists bool
		if err := sql.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM rca_jobs WHERE issue_id=$1)`, issue.id).Scan(&exists); err != nil || exists {
			t.Errorf("excluded Issue %s RCA record=%t, error=%v", issue.id, exists, err)
		}
		return
	}
	if message.SentryURL != issue.link(baseURL) || message.Problem != issue.problem || message.Cause != "保存接口报错" || message.Category != "平台异常" ||
		message.ConfidenceScore == nil || *message.ConfidenceScore != 75 || message.NextStep != integrationNextStep {
		t.Errorf("Issue %s notification lost or mixed evidence: %+v", issue.id, message)
	}
	var project, eventID, storedIssue, cloud string
	var analyses, deliveries int
	err := sql.QueryRow(t.Context(), `SELECT project, event_id, status, report->>'issue_key', report->>'test_cloud', analysis_attempts, delivery_attempts FROM rca_jobs WHERE issue_id=$1`, issue.id).
		Scan(&project, &eventID, &status, &storedIssue, &cloud, &analyses, &deliveries)
	if err != nil || project != issue.project || eventID != issue.eventID() || status != "done" || storedIssue != issue.id || cloud != issue.cloud || analyses != 1 || deliveries != 1 {
		t.Errorf("Issue %s RCA persistence mismatch: project=%q event=%q status=%q issue=%q cloud=%q attempts=%d/%d error=%v",
			issue.id, project, eventID, status, storedIssue, cloud, analyses, deliveries, err)
	}
}
