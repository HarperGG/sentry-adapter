package dingtalk

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const sentryOrigin = "https://sentry.example.com"
const feedbackLink = sentryOrigin + "/organizations/acme/issues/feedback/?feedbackSlug=annotation%3A42&project=5"

func TestSendUsesSanitizedMarkdown(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json; charset=utf-8" {
			t.Errorf("unexpected request method or content type: %s %s", r.Method, r.Header.Get("Content-Type"))
		}
		var body struct {
			robotMessage
			At json.RawMessage `json:"at"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if body.Type != "markdown" || !strings.Contains(body.Markdown.Title, keyword) || len(body.At) != 0 {
			t.Errorf("unexpected message type, title, or mentions: %+v", body)
		}
		for _, want := range []string{
			keyword, "**Issue** 42", `保存\*操作\*失败`, "**初步原因** 读取失败", "**分类** 平台异常",
			"**置信度** 75/100", "**下一步** 核对请求", `\[伪造链接\]`, "（凭据已隐藏）", "（链接已省略）",
			"[查看 Sentry 反馈](" + feedbackLink + ")",
		} {
			if !strings.Contains(body.Markdown.Text, want) {
				t.Errorf("message is missing %q: %s", want, body.Markdown.Text)
			}
		}
		for _, unwanted := range []string{"@", "https://untrusted.example", "test-credential", "\n# 伪造标题", "<img>"} {
			if strings.Contains(body.Markdown.Text, unwanted) {
				t.Errorf("unsanitized text %q in message: %s", unwanted, body.Markdown.Text)
			}
		}
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer server.Close()
	client := testClient(t, server)
	score := 75
	err := client.Send(context.Background(), Report{
		IssueID:         "42",
		Problem:         "保存*操作*失败\n# 伪造标题 @all <img>",
		Cause:           "读取失败，Authorization: Bearer test-credential",
		Category:        "平台异常",
		ConfidenceScore: &score,
		NextStep:        "核对请求 [伪造链接](https://untrusted.example/)",
		SentryURL:       feedbackLink,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
}

func TestSendResponseOutcomes(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		response string
		reject   bool
		retry    bool
	}{
		{"robot rejection", 200, `{"errcode":310000,"errmsg":"do not expose this text"}`, true, false},
		{"robot rate limit", 200, `{"errcode":130101}`, true, true},
		{"HTTP rejection", 403, ``, true, false},
		{"HTTP rate limit", 429, ``, true, true},
		{"gateway error", 502, `{"errcode":0}`, false, false},
		{"HTTP timeout", 408, ``, false, false},
		{"missing errcode", 200, `{"errmsg":"ok"}`, false, false},
		{"null errcode", 200, `{"errcode":null}`, false, false},
		{"string errcode", 200, `{"errcode":"0"}`, false, false},
		{"wrong errcode case", 200, `{"ERRCODE":0}`, false, false},
		{"malformed response", 200, `{"errcode":0`, false, false},
		{"trailing response data", 200, `{"errcode":0} {}`, false, false},
		{"empty response", 204, ``, false, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.response)
			}))
			defer server.Close()
			err := testClient(t, server).Send(context.Background(), Report{IssueID: "42", SentryURL: feedbackLink})
			var rejected *Rejected
			var uncertain *Uncertain
			if test.reject && !errors.As(err, &rejected) || !test.reject && !errors.As(err, &uncertain) {
				t.Fatalf("unexpected outcome (%T): %v", err, err)
			}
			if rejected != nil && rejected.Retryable() != test.retry {
				t.Errorf("retryable = %t, want %t", rejected.Retryable(), test.retry)
			}
			if strings.Contains(err.Error(), "do not expose this text") {
				t.Errorf("response details escaped into error: %v", err)
			}
		})
	}
}

func TestCategoryMessages(t *testing.T) {
	const evidence = "反馈截图含标记"
	score := 30
	for _, test := range []struct {
		kind   string
		want   []string
		absent []string
	}{
		{"platform_bug", []string{"平台异常", "**初步原因** 待验证原因", "**置信度** 低（30/100）"}, []string{"**影响范围**", "**分流依据**"}},
		{"data_issue", []string{"数据问题", "**影响范围** 用户报告第 3 至 5 帧"}, []string{"**初步原因**", "**置信度**", "**分流依据**"}},
		{"other", []string{"其他反馈分流", "**建议分类** 需求建议", "**分流依据** 用户要求增加功能"}, []string{"**初步原因**", "**置信度**", "**影响范围**"}},
	} {
		t.Run(test.kind, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				var payload robotMessage
				if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
					t.Errorf("decode request: %v", err)
					return
				}
				for _, want := range append(test.want, "**分析状态** 部分完成", "**下一步** 请核查", evidence, feedbackLink) {
					if !strings.Contains(payload.Markdown.Text, want) {
						t.Errorf("missing %q from %s", want, payload.Markdown.Text)
					}
				}
				for _, want := range []string{keyword, "TSR", "数据集 123"} {
					if !strings.Contains(payload.Markdown.Title, want) {
						t.Errorf("missing %q from title %s", want, payload.Markdown.Title)
					}
				}
				for _, absent := range append(test.absent, "untrusted.example", "test-credential", "@all") {
					if strings.Contains(payload.Markdown.Text, absent) {
						t.Errorf("unexpected %q in %s", absent, payload.Markdown.Text)
					}
				}
				_, _ = io.WriteString(w, `{"errcode":0}`)
			}))
			defer server.Close()
			report := Report{
				IssueID: "42", FeedbackType: test.kind, TaskType: "TSR", DatasetID: "123", AnalysisStatus: "部分完成",
				Problem: "用户反馈现象", Evidence: evidence + " https://untrusted.example @all access_token=test-credential",
				Cause: "待验证原因", Confidence: "低", ConfidenceScore: &score, Impact: "用户报告第 3 至 5 帧",
				SuggestedCategory: "suggestion", TriageReason: "用户要求增加功能", NextStep: "请核查", SentryURL: feedbackLink,
			}
			if err := testClient(t, server).Send(context.Background(), report); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSendSkipsExcludedCategoriesBeforePosting(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	client := testClient(t, server)
	for _, kind := range []string{"suggestion", "feature_gap", "unexpected"} {
		var rejected *Rejected
		err := client.Send(context.Background(), Report{IssueID: "42", FeedbackType: kind, SentryURL: feedbackLink})
		if !errors.As(err, &rejected) || rejected.Retryable() {
			t.Errorf("excluded category %s produced %v", kind, err)
		}
	}
	if requests.Load() != 0 {
		t.Errorf("made %d robot requests for excluded categories", requests.Load())
	}
}

func TestReportTextKeepsDatasetAndFindingsWhileRemovingInternalIDs(t *testing.T) {
	tests := []struct{ input, want string }{
		{"限定于标注上下文 clipsId=abcdef010101010101010101、datasetId=573124、dataPoolId=abcdef0202020202、requirementId=abcdef030303030303030303 的 fov120 视角",
			"限定于标注上下文 数据集 573124 的 fov120 视角"},
		{"对 clipsId abcdef010101010101010101（datasetId 573124，requirement abcdef030303030303030303）fov120 视角核对第 3 至 5 帧",
			"对 该任务（数据集 573124）fov120 视角核对第 3 至 5 帧"},
		{"在 replay b44d… 中发现请求返回 HTTP 403", "在 该回放 中发现请求返回 HTTP 403"},
		{"按 replay_id b44d...abc9 查看 15 秒回放", "按 该回放 查看 15 秒回放"},
		{"按 replay_id abcdef00-aaaa-1111-0000-bbbbbbbbbbbb 查看", "按 该回放 查看"},
		{"见 https://internal.example/clip?datasetId=573124&sig=test-material", "见 （链接已省略）"},
		{"核查第 12345678 帧和状态码 503", "核查第 12345678 帧和状态码 503"},
		{"受影响：datasetId=573124、clipsId=abcdef0101010101、datasetId=573125", "受影响：数据集 573124、573125"},
	}
	for _, test := range tests {
		if got := reportText(test.input, ""); got != test.want {
			t.Errorf("reportText(%q)=%q, want %q", test.input, got, test.want)
		}
	}
}

func TestSendDoesNotForwardRedirectOrUseUnverifiedLink(t *testing.T) {
	var requests, forwarded atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		forwarded.Add(1)
		_, _ = io.WriteString(w, `{"errcode":0}`)
	}))
	defer target.Close()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client := testClient(t, server)
	var rejected *Rejected
	if err := client.Send(context.Background(), Report{IssueID: "42", SentryURL: "https://untrusted.example/42/"}); !errors.As(err, &rejected) {
		t.Fatalf("unverified link outcome: %v", err)
	}
	if requests.Load() != 0 {
		t.Fatal("request sent for unverified link")
	}
	var uncertain *Uncertain
	if err := client.Send(context.Background(), Report{IssueID: "42", SentryURL: feedbackLink}); !errors.As(err, &uncertain) {
		t.Fatalf("redirect outcome: %v", err)
	}
	if requests.Load() != 1 || forwarded.Load() != 0 {
		t.Fatalf("webhook requests %d, forwarded %d", requests.Load(), forwarded.Load())
	}
}

func TestTransportErrorKeepsWebhookCredentialOutOfError(t *testing.T) {
	client, err := New("https://robot.example.com/send?access_token=test-webhook-credential", sentryOrigin)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = client.Send(ctx, Report{IssueID: "42", SentryURL: feedbackLink})
	var uncertain *Uncertain
	if !errors.As(err, &uncertain) || strings.Contains(err.Error(), "test-webhook-credential") {
		t.Fatalf("unredacted or unexpected error: %v", err)
	}
}

func testClient(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	client, err := New(server.URL+"/robot/send?access_token=test-token", sentryOrigin)
	if err != nil {
		t.Fatalf("create test client: %v", err)
	}
	client.http.Transport = server.Client().Transport
	return client
}
