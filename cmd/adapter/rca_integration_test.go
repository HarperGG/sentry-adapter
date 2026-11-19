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
	"slices"
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
	integrationLinkedErrorID   = "1234567890abcdef1234567890abcdef"
	integrationMissingErrorID  = "9876543210abcdef9876543210abcdef"
	integrationFailureStage    = "open.resources"
	integrationExceptionValue  = "获取相机配置文件失败"
	integrationHookKey         = "local-test-signing-key"
	integrationNextStep        = "核对保存请求的错误栈"
	integrationDataImpact      = "用户报告第 3 至 5 帧缺失"
	integrationTriageReason    = "用户希望增加一个新的操作"
)

type integrationIssue struct {
	id, project, projectID, cloud, environment, kind, problem string
	linkedErrorID                                             string
	wantCard, wantRCA                                         bool
}

func (i integrationIssue) eventID() string { return strings.Repeat("a", 30) + i.id }

func (i integrationIssue) link(baseURL string) string {
	return baseURL + "/organizations/" + integrationOrg + "/issues/feedback/?feedbackSlug=" +
		url.QueryEscape(i.project+":"+i.id) + "&project=" + i.projectID
}

func TestSignedFeedbackRoutesRCAIndependentlyOfTeambitionProject(t *testing.T) {
	db, sql := openIntegrationStore(t)
	otherIssue := integrationIssue{id: "51", project: integrationAnalysisProject, projectID: "8", kind: "other", problem: "希望能按帧批量选中", wantRCA: true}
	issues := []integrationIssue{
		{id: "42", project: integrationCardProject, projectID: "5", cloud: "TC", environment: "production", kind: "platform_bug", problem: "腾讯云保存失败", wantCard: true},
		{id: "43", project: integrationAnalysisProject, projectID: "8", cloud: "HS", environment: "test", kind: "platform_bug", problem: "火山云保存失败", linkedErrorID: integrationLinkedErrorID, wantRCA: true},
		{id: "44", project: integrationCardProject, projectID: "5", cloud: "HS", environment: "production", kind: "data_issue", problem: "标注数据缺失", wantCard: true},
		{id: "45", project: integrationAnalysisProject, projectID: "8", cloud: "TC", environment: "test", kind: "suggestion", problem: "增加批量保存入口"},
		{id: "46", project: integrationAnalysisProject, projectID: "8", environment: "production", kind: "platform_bug", problem: "测试项目携带生产标签", linkedErrorID: integrationMissingErrorID, wantRCA: true},
		{id: "47", project: integrationCardProject, projectID: "5", kind: "other", problem: "其他反馈", wantCard: true},
		{id: "48", project: integrationCardProject, projectID: "5", cloud: "HS", environment: "test", kind: "platform_bug", problem: "线上项目携带测试标签", wantCard: true},
		{id: "49", project: integrationAnalysisProject, projectID: "8", cloud: "TC", kind: "platform_bug", problem: "测试反馈缺少环境标签", wantRCA: true},
		{id: "50", project: integrationAnalysisProject, projectID: "8", kind: "data_issue", problem: "第 3 至 5 帧的点云缺失", linkedErrorID: integrationLinkedErrorID, wantRCA: true},
		otherIssue,
		{id: "52", project: integrationCardProject, projectID: "5", kind: "suggestion", problem: "希望新增快捷键", wantCard: true},
		{id: "53", project: integrationAnalysisProject, projectID: "8", kind: "feature_gap", problem: "历史版本缺少入口"},
		{id: "54", project: integrationAnalysisProject, projectID: "8", kind: "unmapped", problem: "未知分类"},
		{id: "55", project: integrationAnalysisProject, projectID: "8", problem: "缺失分类"},
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
	rcaWorker := worker.RCAWorker{Store: db, Sentry: sentryClient, Agent: agent, Robot: messages, AllowedProjects: cfg.RCAAllowedProjects, Logger: logger}
	workers.Go(func() { feedbackWorker.Run(ctx) })
	workers.Go(func() { rcaWorker.Run(ctx) })
	waitIntegrationJobs(t, sql, len(issues), expectedAnalyses)

	// The alternate webhook source delivers the same issue after both workers finish.
	second := otherIssue
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

func TestDisabledAnalysisStillCreatesFeedbackCards(t *testing.T) {
	db, sql := openIntegrationStore(t)
	issues := []integrationIssue{
		{id: "61", project: integrationCardProject, projectID: "5", kind: "platform_bug", problem: "保存失败", linkedErrorID: integrationLinkedErrorID},
		{id: "62", project: integrationCardProject, projectID: "5", kind: "data_issue", problem: "数据偏移"},
		{id: "63", project: integrationCardProject, projectID: "5", kind: "other", problem: "工作流疑问"},
		{id: "64", project: integrationCardProject, projectID: "5", kind: "suggestion", problem: "操作建议"},
	}
	server := newIntegrationSentry(t, issues)
	sentryClient, err := sentry.New(server.URL, "local-test-token")
	if err != nil {
		t.Fatal(err)
	}
	rules, err := routing.Load("../../config/feedback-routing.json")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(t.Output(), nil))
	cfg := config.Config{
		WebhookSecret: integrationHookKey, SentryOrg: integrationOrg, SentryProject: integrationCardProject,
		RCAEnabled: false, RCAAllowedProjects: []string{integrationCardProject},
	}
	handler := feedbackHandler(cfg, db, logger)
	for _, issue := range issues {
		postIntegrationWebhook(t, handler, "issue", integrationIssueDelivery(issue, integrationOrg), integrationHookKey, http.StatusAccepted)
	}
	cards := make(integrationTeambition, len(issues))
	w := worker.Worker{
		Store: db, Sentry: sentryClient, Teambition: cards, Routing: rules,
		Organization: integrationOrg, Project: integrationCardProject, RCAEnabled: false,
		RCAAllowedProjects: cfg.RCAAllowedProjects, Logger: logger,
	}
	ctx, cancel := context.WithCancel(t.Context())
	var workers sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		workers.Wait()
	})
	workers.Go(func() { w.Run(ctx) })
	waitIntegrationJobs(t, sql, len(issues), 0)
	if len(cards) != len(issues) {
		t.Fatalf("created %d cards with analysis disabled, want %d", len(cards), len(issues))
	}
	var queued int
	if err := sql.QueryRow(t.Context(), "SELECT count(*) FROM rca_jobs").Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("analysis jobs with switch off = %d, error=%v", queued, err)
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
	var mu sync.Mutex
	reads := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/0/projects/") {
			mu.Lock()
			reads[r.Method+" "+r.URL.Path]++
			mu.Unlock()
			if r.URL.Path == "/api/0/projects/"+integrationOrg+"/"+integrationAnalysisProject+"/events/"+integrationLinkedErrorID+"/" {
				_, _ = fmt.Fprintf(w, `{"eventID":%q,"type":"error","tags":[{"key":"failure_stage","value":%q}],"entries":[{"type":"exception","data":{"values":[{"type":"CalibrationResourceError","value":%q}]}}],"user":{"email":"private-field@example.invalid"}}`,
					integrationLinkedErrorID, integrationFailureStage, integrationExceptionValue+"（/test-bucket/input/cameras.cfg）: HTTP status 404")
			} else {
				http.NotFound(w, r)
			}
			return
		}
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
				tags := []map[string]string{{"key": "feedback_type", "value": issue.kind}, {"key": "task_type", "value": "OD"}, {"key": "dataset_id", "value": "dataset-" + issue.id}}
				feedback := map[string]string{"message": issue.problem, "url": businessURL}
				if issue.linkedErrorID != "" && issue.kind == "data_issue" {
					tags = append(tags, map[string]string{"key": "associated_event_id", "value": issue.linkedErrorID})
				} else if issue.linkedErrorID != "" {
					feedback["associated_event_id"] = issue.linkedErrorID
				}
				event := map[string]any{
					"eventID":  issue.eventID(),
					"contexts": map[string]any{"feedback": feedback},
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
	t.Cleanup(func() {
		server.Close()
		want := make(map[string]int)
		for _, issue := range issues {
			if issue.wantRCA && issue.linkedErrorID != "" {
				want["GET /api/0/projects/"+integrationOrg+"/"+issue.project+"/events/"+issue.linkedErrorID+"/"]++
			}
		}
		if !reflect.DeepEqual(reads, want) {
			t.Errorf("project event lookups = %v, want %v", reads, want)
		}
	})
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
				if issue.wantRCA && strings.Contains(req["message"], link) {
					assertAgentEventReferences(t, issue, req["message"])
					report := integrationAgentReport(t, issue, link)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "data: {\"type\":\"text\",\"content\":%s}\n\nevent:done\ndata:{\"type\":\"done\",\"reason\":\"completed\"}\n\n", jsonString(report))
					return
				}
			}
			t.Error("AgentOS analysis lost the Sentry link or analyzed an excluded issue")
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	return server, sessionCount
}

func assertAgentEventReferences(t *testing.T, issue integrationIssue, message string) {
	t.Helper()
	lines := "\n" + message + "\n"
	for _, required := range []string{
		"feedback_type=" + issue.kind,
		"project_slug=" + issue.project,
		"反馈事件本身的 Event ID：" + issue.eventID(),
	} {
		if !strings.Contains(lines, "\n"+required+"\n") {
			t.Errorf("Issue %s AgentOS request missing %q", issue.id, required)
		}
	}
	var linked, observations []string
	for line := range strings.SplitSeq(message, "\n") {
		if id, ok := strings.CutPrefix(line, "associated_event_id="); ok {
			linked = append(linked, id)
		}
		if raw, ok := strings.CutPrefix(line, "linked_error_observation="); ok {
			observations = append(observations, raw)
		}
	}
	var expected []string
	if issue.linkedErrorID != "" {
		expected = []string{issue.linkedErrorID}
	}
	if !slices.Equal(linked, expected) {
		t.Errorf("Issue %s associated errors in AgentOS request = %q, expected %q", issue.id, linked, expected)
	}
	if issue.linkedErrorID != integrationLinkedErrorID {
		if len(observations) != 0 || strings.Contains(message, integrationExceptionValue) {
			t.Errorf("Issue %s sent observations without reading a matching error", issue.id)
		}
		return
	}
	var observation map[string]any
	if len(observations) != 1 || json.Unmarshal([]byte(observations[0]), &observation) != nil {
		t.Errorf("Issue %s must send one JSON observation for its linked error", issue.id)
		return
	}
	if observation["event_id"] != issue.linkedErrorID || observation["failure_stage"] != integrationFailureStage ||
		observation["exception_type"] != "CalibrationResourceError" || observation["exception_value"] != integrationExceptionValue ||
		observation["resource_file"] != "cameras.cfg" || observation["http_status"] != "404" ||
		strings.Contains(message, "/test-bucket/input/") || strings.Contains(message, "private-field@example.invalid") {
		t.Errorf("Issue %s did not send the scoped linked error diagnosis: %v", issue.id, observation)
	}
}

func integrationAgentReport(t *testing.T, issue integrationIssue, link string) string {
	t.Helper()
	summary := map[string]string{"description": issue.problem}
	verification := integrationNextStep
	report := map[string]any{
		"analysis_status": "部分完成", "issue_key": issue.id, "feedback_summary": summary, "test_cloud": issue.cloud,
		"evidence": []map[string]string{{"observation": issue.problem, "reference": link}},
	}
	switch issue.kind {
	case "platform_bug":
		// The deployed platform report predates feedback_type.
		report["root_cause"] = map[string]any{"conclusion": "保存接口报错", "category": "平台异常", "confidence": "中", "confidence_score": 60}
	case "data_issue":
		report["feedback_type"] = issue.kind
		summary["impact"] = integrationDataImpact
		verification = "核对这三帧的原始数据"
	case "other":
		report["feedback_type"] = issue.kind
		report["triage"] = map[string]string{"suggested_category": "suggestion", "reason": integrationTriageReason}
		verification = "转产品核对操作场景"
	}
	report["recommendations"] = map[string][]string{"verification_steps": {verification}}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Errorf("encode fixture report: %v", err)
	}
	return string(raw)
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
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
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
	if message.SentryURL != issue.link(baseURL) || message.Problem != issue.problem || message.FeedbackType != issue.kind ||
		message.DatasetID != "dataset-"+issue.id || message.TaskType != "OD" || message.AnalysisStatus != "部分完成" || !strings.Contains(message.Evidence, issue.problem) {
		t.Errorf("Issue %s notification lost or mixed evidence: %+v", issue.id, message)
	}
	switch issue.kind {
	case "platform_bug":
		if message.Cause != "保存接口报错" || message.Category != "平台异常" || message.Confidence != "中" || message.ConfidenceScore == nil || *message.ConfidenceScore != 60 || message.NextStep != integrationNextStep {
			t.Errorf("Issue %s lost platform diagnosis: %+v", issue.id, message)
		}
	case "data_issue":
		if message.Impact != integrationDataImpact || message.Cause != "" || message.ConfidenceScore != nil || message.NextStep != "核对这三帧的原始数据" {
			t.Errorf("Issue %s lost data impact: %+v", issue.id, message)
		}
	case "other":
		if message.SuggestedCategory != "suggestion" || message.TriageReason != integrationTriageReason || message.Cause != "" || message.ConfidenceScore != nil || message.NextStep != "转产品核对操作场景" {
			t.Errorf("Issue %s lost triage recommendation: %+v", issue.id, message)
		}
	}
	var project, eventID, linkedErrorID, storedIssue, cloud string
	var analyses, deliveries int
	err := sql.QueryRow(t.Context(), `SELECT project, event_id, associated_error_id, status, report->>'issue_key', report->>'test_cloud', analysis_attempts, delivery_attempts FROM rca_jobs WHERE issue_id=$1`, issue.id).
		Scan(&project, &eventID, &linkedErrorID, &status, &storedIssue, &cloud, &analyses, &deliveries)
	if err != nil || project != issue.project || eventID != issue.eventID() || linkedErrorID != issue.linkedErrorID || status != "done" || storedIssue != issue.id || cloud != issue.cloud || analyses != 1 || deliveries != 1 {
		t.Errorf("Issue %s RCA persistence mismatch: project=%q event=%q associated error=%q status=%q issue=%q cloud=%q attempts=%d/%d error=%v",
			issue.id, project, eventID, linkedErrorID, status, storedIssue, cloud, analyses, deliveries, err)
	}
}
