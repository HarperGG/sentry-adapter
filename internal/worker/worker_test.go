package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"sentry-adapter/internal/routing"
	"sentry-adapter/internal/sentry"
	"sentry-adapter/internal/teambition"
)

func TestCardPreservesOriginalFeedbackWithoutUploadingImages(t *testing.T) {
	f := sentry.Feedback{
		IssueID: "6341", TaskType: "OD_correct", DatasetID: "odc_0908-JGzm",
		Kind: "platform_bug", Username: "admin",
		Message:     "[反馈类型] 平台异常\n\n[问题描述]\n打开页面后无法保存 ![截图](https://example.invalid/a.png)",
		IssueURL:    "https://sentry.example/issues/6341/",
		Attachments: []sentry.Attachment{{ID: "screenshot-id", Name: "screenshot.png", MimeType: "image/png", Size: 100}},
	}
	decision := routing.Decision{FeedbackType: "platform_bug", TagID: "platform-tag", ExecutorID: "owner"}
	card := cardFromFeedback(f, decision)
	if card.Description != f.Message+"\n\n"+f.IssueURL+"\n\n用户名：admin" {
		t.Errorf("description must preserve feedback, links, and username: %q", card.Description)
	}
	if card.Title != "【OD_correct: odc_0908-JGzm】 打开页面后无法保存" {
		t.Errorf("title = %q", card.Title)
	}
	if card.SourceIssueID != f.IssueID || card.FeedbackType != "platform_bug" || card.ExecutorID != "owner" ||
		!reflect.DeepEqual(card.TagIDs, []string{"platform-tag"}) || card.Kind != "" {
		t.Errorf("unexpected card identity and routing: %+v", card)
	}
	if len(card.Images) != 0 {
		t.Errorf("images = %d, want zero even when feedback has attachments", len(card.Images))
	}
}

func TestCardWithMissingMetadataStillHasDescriptionAndTitle(t *testing.T) {
	f := sentry.Feedback{Message: "反馈内容", IssueURL: "https://sentry.example/issues/42/"}
	card := cardFromFeedback(f, routing.Decision{FeedbackType: "other", TagID: "other-tag"})
	if card.Description != "反馈内容\n\nhttps://sentry.example/issues/42/" {
		t.Errorf("description = %q", card.Description)
	}
	if card.Title != "【未知任务类型: 未知数据集】 反馈内容" || card.ExecutorID != "" {
		t.Errorf("missing metadata should still produce an unassigned card: %+v", card)
	}
}

func TestConfiguredRoutingReachesTaskCreateRequest(t *testing.T) {
	rules, err := routing.Load("../../config/feedback-routing.json")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, kind, taskType, tagID, executorID string
	}{
		{"data issue", "data_issue", "OD", "6ae048f9e39f6320c05f93d7", "68707b9783c2517553ea6203"},
		{"data missing task", "data_issue", "", "6ae048f9e39f6320c05f93d7", "68707b9783c2517553ea6203"},
		{"data unconfigured task", "data_issue", "PD_label", "6ae048f9e39f6320c05f93d7", "68707b9783c2517553ea6203"},
		{"suggestion", "suggestion", "OD", "6ae0490edcbd5c5e9e53d410", "6a277b7030ff5045c8d8bfd8"},
		{"suggestion missing task", "suggestion", "", "6ae0490edcbd5c5e9e53d410", "6a277b7030ff5045c8d8bfd8"},
		{"suggestion unconfigured task", "suggestion", "PD_label", "6ae0490edcbd5c5e9e53d410", "6a277b7030ff5045c8d8bfd8"},
		{"other", "other", "OD", "6ae04920131cc905fc5a7dac", ""},
		{"point cloud", "platform_bug", "Lidar_seg", "6ae0449c6ce07cfbf775b7c4", "68e8d3202e316da74b8189cc"},
		{"4D", "platform_bug", "PSD", "6ae0449c6ce07cfbf775b7c4", "6889c08f7c2784422c96ae0f"},
		{"2D case alias", "platform_bug", "vlm_text_sequence", "6ae0449c6ce07cfbf775b7c4", "69ce485f652bf1ac6daa9b05"},
		{"OD", "platform_bug", "OD_4D", "6ae0449c6ce07cfbf775b7c4", "67c53de0c45defb1e0693426"},
		{"unconfigured", "platform_bug", "PD_label", "6ae0449c6ce07cfbf775b7c4", ""},
		{"missing metadata", "platform_bug", "", "6ae0449c6ce07cfbf775b7c4", ""},
		{"ambiguous legacy", "feature_gap", "OD", "6ae04920131cc905fc5a7dac", ""},
		{"unknown", "future_type", "OD", "6ae04920131cc905fc5a7dac", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var request map[string]json.RawMessage
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/gateway/v3/task/create" {
					t.Errorf("unexpected request path: %s", r.URL.Path)
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"result":{"taskId":"created-task"}}`))
			}))
			defer server.Close()
			client, err := teambition.New(teambition.Config{
				BaseURL: server.URL, TaskPath: "/gateway/v3/task/create", AuthMode: "local_jwt",
				AppID: "test-app", AppSecret: "test-only-secret", ProjectID: "project",
				ScenarioID: "shared-scenario", ExecutorID: "must-not-be-used",
			})
			if err != nil {
				t.Fatal(err)
			}
			feedback := sentry.Feedback{
				IssueID: "42", TaskType: tc.taskType, Kind: tc.kind,
				Message:  "[反馈类型] 原始分类\n\n[问题描述]\n点击保存后没有响应 ![截图](/image.png)",
				IssueURL: "https://sentry.example/issues/42/",
			}
			card := cardFromFeedback(feedback, rules.Resolve(feedback.Kind, feedback.TaskType))
			prepared, err := client.Prepare(context.Background(), card)
			if err != nil {
				t.Fatal(err)
			}
			id, err := client.Create(context.Background(), prepared)
			if err != nil || id != "created-task" || calls != 1 {
				t.Fatalf("Create() = %q, %v, calls %d", id, err, calls)
			}
			var tags []string
			if err := json.Unmarshal(request["tagIds"], &tags); err != nil || !reflect.DeepEqual(tags, []string{tc.tagID}) {
				t.Fatalf("tagIds = %s, error %v", request["tagIds"], err)
			}
			var executor string
			if raw, exists := request["executorId"]; exists {
				if err := json.Unmarshal(raw, &executor); err != nil || tc.executorID == "" {
					t.Fatalf("unexpected executorId: %s", raw)
				}
			}
			if executor != tc.executorID {
				t.Errorf("executorId = %q, want %q", executor, tc.executorID)
			}
			var content, note string
			if err := json.Unmarshal(request["content"], &content); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(request["note"], &note); err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(content, "】 点击保存后没有响应") || !strings.Contains(note, feedback.Message) {
				t.Errorf("title or original note changed: content=%q note=%q", content, note)
			}
		})
	}
}
