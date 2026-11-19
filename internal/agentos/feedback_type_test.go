package agentos

import (
	"encoding/json"
	"strings"
	"testing"
)

const dataReport = `{"analysis_status":"部分完成","feedback_type":"data_issue","feedback_summary":{"description":"用户报告后视图偏移","impact":"用户报告第3至8帧，其他帧待核实"},"evidence":[{"observation":"反馈列出了帧范围","reference":"反馈事件 F1"}],"recommendations":{"verification_steps":["对照原始帧"]}}`
const triageReport = `{"analysis_status":"分析完成","feedback_type":"other","feedback_summary":{"description":"希望一次操作选择多个对象"},"triage":{"suggested_category":"suggestion","reason":"用户希望新增批量选择"},"evidence":[{"observation":"用户提出新交互","reference":"反馈事件 F2"}],"recommendations":{"verification_steps":["转产品确认适用场景"]}}`

func TestCategoryReportsWithoutRootCause(t *testing.T) {
	for _, tc := range []struct{ kind, report string }{{"data_issue", dataReport}, {"other", triageReport}} {
		stream := textFrame("分析结果\n```json\n"+tc.report+"\n```") + doneFrame
		raw, err := readReportStream(strings.NewReader(stream), tc.kind)
		if err != nil {
			t.Fatal(err)
		}
		r, err := ParseReportForType(raw, tc.kind)
		if err != nil || !r.Notifiable() {
			t.Fatalf("%s could not deliver a report without root cause: %v", tc.kind, err)
		}
	}
}

func TestRejectsMismatchedAndUnactionableReports(t *testing.T) {
	for _, tc := range []struct{ name, kind, report string }{
		{"platform response for data", "data_issue", reportJSON},
		{"triage response for data", "data_issue", triageReport},
		{"missing impact", "data_issue", strings.Replace(dataReport, "用户报告第3至8帧，其他帧待核实", "", 1)},
		{"invalid routing", "other", strings.Replace(triageReport, "suggestion", "product", 1)},
		{"no routing rationale", "other", strings.Replace(triageReport, "用户希望新增批量选择", "", 1)},
		{"no verification", "other", strings.Replace(triageReport, "转产品确认适用场景", "", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseReportForType(json.RawMessage(tc.report), tc.kind); err == nil {
				t.Fatal("accepted incompatible report")
			}
		})
	}
}

func TestEvidenceForNotification(t *testing.T) {
	r, err := ParseReportForType(json.RawMessage(dataReport), "data_issue")
	if err != nil {
		t.Fatal(err)
	}
	r.Evidence = append([]Evidence{{Observation: "没有来源"}}, r.Evidence...)
	e, ok := r.FirstEvidence()
	if !ok || e.Reference != "反馈事件 F1" {
		t.Fatalf("selected a reference-less observation: %+v", e)
	}
	r.Evidence = r.Evidence[:1]
	if r.Notifiable() {
		t.Fatal("accepted evidence with no source")
	}
}

func TestAnalysisPromptFollowsVerifiedFeedbackType(t *testing.T) {
	for kind, field := range map[string]string{"platform_bug": "root_cause", "data_issue": "feedback_summary.impact", "other": "triage.suggested_category"} {
		message, err := analysisMessage("123", "https://sentry.example/issues/123/", "F1", kind, FeedbackSource{})
		if err != nil || !strings.Contains(message, "feedback_type="+kind) || !strings.Contains(message, "反馈事件本身的 Event ID：F1") || !strings.Contains(message, field) {
			t.Fatalf("incomplete %s task: %v", kind, err)
		}
		if strings.Contains(message, "\nassociated_event_id=") || !strings.Contains(message, "contexts.feedback_details.associated_error_id") {
			t.Errorf("%s task without a stored association must check the original event", kind)
		}
	}
	for _, kind := range []string{"suggestion", "", "unknown"} {
		if _, err := analysisMessage("123", "", "", kind, FeedbackSource{}); err == nil {
			t.Fatalf("accepted analysis for %q", kind)
		}
	}
}
