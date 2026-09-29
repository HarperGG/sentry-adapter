package worker

import (
	"strings"
	"testing"

	"sentry-adapter/internal/sentry"
)

func TestTitleUsesSpecificFeedbackType(t *testing.T) {
	cases := []struct {
		name, issueTitle, feedbackType, want string
	}{
		{"platform bug", "User Feedback", "platform_bug", "【Sentry】【平台缺陷】"},
		{"feature gap", "上传图片时失败", "feature_gap", "【Sentry】【功能缺失】 上传图片时失败"},
		{"suggestion", "用户反馈", "suggestion", "【Sentry】【建议】"},
		{"other", "反馈   说明", "other", "【Sentry】【其他】 反馈 说明"},
		{"bug alias", "", "bug", "【Sentry】【缺陷】"},
		{"feature alias", "Feedback", "request", "【Sentry】【需求】"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := title(tc.issueTitle, tc.feedbackType); got != tc.want {
				t.Errorf("title() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTitleKeepsRuneLimit(t *testing.T) {
	got := title(strings.Repeat("问", 120), "platform_bug")
	if len([]rune(got)) != 100 || !strings.HasSuffix(got, "…") {
		t.Errorf("title() = %q; want 100 runes ending in ellipsis", got)
	}
}

func TestCardContainsOnlyFeedbackURLAndNoImages(t *testing.T) {
	feedback := sentry.Feedback{
		IssueID:  "6341",
		Title:    "上传图片时失败",
		Kind:     "platform_bug",
		Message:  "打开页面后无法保存",
		IssueURL: "https://sentry.example.com/organizations/sentry/issues/feedback/?feedbackSlug=anno-dev%3A6341&project=5",
		Attachments: []sentry.Attachment{{
			ID: "screenshot-id", Name: "screenshot.png", MimeType: "image/png", Size: 100,
		}},
	}
	card := cardFromFeedback(feedback, classify(feedback.Kind))
	if card.Description != feedback.IssueURL {
		t.Errorf("description = %q, want exact feedback URL %q", card.Description, feedback.IssueURL)
	}
	if card.Title != "【Sentry】【平台缺陷】 上传图片时失败" {
		t.Errorf("title = %q", card.Title)
	}
	if card.SourceIssueID != feedback.IssueID || card.Kind != "bug" {
		t.Errorf("card identity/kind = %+v", card)
	}
	if len(card.Images) != 0 {
		t.Errorf("images = %d, want zero even when feedback has attachments", len(card.Images))
	}
}
