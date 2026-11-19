package agentos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const reportJSON = `{"analysis_status":"部分完成","issue_key":"DEMO-17","feedback_summary":{"description":"页面保存失败"},"root_cause":{"conclusion":"响应超时，需对照服务日志","category":"无法判定","confidence":"低","confidence_score":30},"evidence":[{"observation":"请求耗时超过阈值","reference":"event-17"}],"recommendations":{"verification_steps":["核对服务端耗时"]},"missing_context":["服务端日志"]}`

func textFrame(s string) string {
	b, _ := json.Marshal(map[string]string{"type": "text", "content": s})
	return "data: " + string(b) + "\n\n"
}

const doneFrame = "event:done\ndata:{\"type\":\"done\",\"reason\":\"completed\"}\n\n"

func TestAnalyzePublishedWireFormat(t *testing.T) {
	const base = "/armory-core/api/v1"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input map[string]string
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer test-key" || input["agentId"] != "200004" || input["userId"] != "sentry-adapter" {
			t.Error("unexpected AgentOS request")
		}
		switch r.URL.Path {
		case base + "/create_session":
			_, _ = w.Write([]byte(`{"code":"0000","info":"ok","data":{"sessionId":"session-17"}}`))
		case base + "/chat_stream":
			if input["sessionId"] != "session-17" || !strings.Contains(input["message"], "Issue 17") || !strings.Contains(input["message"], "event-17") || r.Header.Get("Accept") != "text/event-stream" {
				t.Error("chat request missing verified issue context or session")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, ": keepalive\n\nevent:thinking\ndata:{\"type\":\"thinking\",\"phase\":\"start\"}\n\n")
			_, _ = fmt.Fprint(w, textFrame("### 问题研判\n- 问题 保存失败\n\n```json\n"))
			_, _ = fmt.Fprint(w, textFrame(reportJSON[:15]))
			_, _ = fmt.Fprint(w, "event:tool_end\ndata:{\"type\":\"tool_end\",\"status\":\"error\",\"content\":{\"private\":true}}\n\n")
			_, _ = fmt.Fprint(w, textFrame(reportJSON[15:]+"\n```"), doneFrame)
		default:
			t.Error("unexpected endpoint")
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	c, err := New(server.URL+base+"/", "200004", "sentry-adapter", "test-key")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := c.Analyze(context.Background(), "17", "https://sentry.example/issues/17/", "event-17", "platform_bug")
	if err != nil || string(raw) != reportJSON {
		t.Fatalf("report mismatch (%v)", err)
	}
}

func TestStreamRequiresCompletedAssistantReport(t *testing.T) {
	for name, stream := range map[string]string{
		"incomplete": textFrame(reportJSON),
		"failed":     textFrame(reportJSON) + "event:done\ndata:{\"type\":\"done\",\"reason\":\"failed\"}\n\n",
		"error":      textFrame(reportJSON) + "event:error\ndata:{}\n\n",
		"tool":       "event:tool_end\n" + textFrame(reportJSON) + doneFrame,
		"stale":      textFrame("```json\n"+reportJSON+"\n```\n取证失败。") + doneFrame,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := readReportStream(strings.NewReader(stream), "platform_bug"); err == nil {
				t.Fatal("accepted unusable report")
			}
		})
	}
}

func TestSessionFailureStopsBeforeChat(t *testing.T) {
	var chatCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/create_session" {
			chatCalls++
		}
		_, _ = w.Write([]byte(`{"code":"4010","info":"rejected","data":null}`))
	}))
	defer server.Close()
	client, err := New(server.URL, "200004", "sentry-adapter", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Analyze(context.Background(), "17", "", "", "platform_bug")
	if err == nil || chatCalls != 0 {
		t.Fatalf("err=%v, chat calls=%d", err, chatCalls)
	}
}

func TestHTTPStatusPreserved(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	client, err := New(server.URL, "200004", "sentry-adapter", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Analyze(context.Background(), "17", "", "", "platform_bug")
	var api *APIError
	if !errors.As(err, &api) || api.Status != 429 || !api.Retryable() {
		t.Fatalf("got %v", err)
	}
}

func TestReportEligibility(t *testing.T) {
	for _, status := range []string{"分析失败", "信息不足"} {
		b, _ := json.Marshal(map[string]string{"analysis_status": status})
		r, err := ParseReportForType(b, "platform_bug")
		if err != nil || r.Notifiable() {
			t.Fatalf("status %s: notifiable=%v, err=%v", status, r.Notifiable(), err)
		}
	}
	for _, bad := range []string{`{}`, `{"analysis_status":"success"}`, `{"analysis_status":"分析完成"}`} {
		if _, err := ParseReportForType([]byte(bad), "platform_bug"); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	r, err := ParseReportForType([]byte(reportJSON), "platform_bug")
	if err != nil || !r.Notifiable() {
		t.Fatalf("sample report: notifiable=%v, err=%v", r.Notifiable(), err)
	}
	r.Evidence = nil
	if r.Notifiable() {
		t.Fatal("empty evidence marked as notifiable")
	}
}
