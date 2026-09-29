package worker

import (
	"strings"
	"testing"

	"sentry-adapter/internal/sentry"
)

func TestTitleUsesTaskMetadataAndFeedbackType(t *testing.T) {
	cases := []struct {
		name, feedbackType, want string
	}{
		{"platform bug", "platform_bug", "【OD_correct: odc_0908-JGzm】 平台异常"},
		{"feature gap", "feature_gap", "【OD_correct: odc_0908-JGzm】 功能不符合预期"},
		{"suggestion", "suggestion", "【OD_correct: odc_0908-JGzm】 使用建议"},
		{"other", "other", "【OD_correct: odc_0908-JGzm】 其他"},
		{"bug alias", "bug", "【OD_correct: odc_0908-JGzm】 缺陷"},
		{"feature alias", "request", "【OD_correct: odc_0908-JGzm】 需求"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := title("OD_correct", "odc_0908-JGzm", tc.feedbackType); got != tc.want {
				t.Errorf("title() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTitleKeepsRuneLimit(t *testing.T) {
	got := title(strings.Repeat("任务", 60), "odc_0908-JGzm", "platform_bug")
	if len([]rune(got)) != 100 || !strings.HasSuffix(got, "】 平台异常") || !strings.Contains(got, "…") {
		t.Errorf("title() = %q; want 100 runes with a truncated prefix and complete type", got)
	}
}

func TestCardContainsFeedbackMessageAndURLWithoutImages(t *testing.T) {
	feedback := sentry.Feedback{
		IssueID:   "6341",
		TaskType:  "OD_correct",
		DatasetID: "odc_0908-JGzm",
		Kind:      "platform_bug",
		Message:   "[反馈类型] 平台异常\n\n[问题描述]\n打开页面后无法保存",
		IssueURL:  "https://sentry.example.com/organizations/sentry/issues/feedback/?feedbackSlug=anno-dev%3A6341&project=5",
		Attachments: []sentry.Attachment{{
			ID: "screenshot-id", Name: "screenshot.png", MimeType: "image/png", Size: 100,
		}},
	}
	card := cardFromFeedback(feedback, classify(feedback.Kind))
	if card.Description != feedback.Message+"\n\n"+feedback.IssueURL {
		t.Errorf("description = %q, want feedback message followed by URL", card.Description)
	}
	if card.Title != "【OD_correct: odc_0908-JGzm】 平台异常" {
		t.Errorf("title = %q", card.Title)
	}
	if card.SourceIssueID != feedback.IssueID || card.Kind != "bug" {
		t.Errorf("card identity/kind = %+v", card)
	}
	if len(card.Images) != 0 {
		t.Errorf("images = %d, want zero even when feedback has attachments", len(card.Images))
	}
}
