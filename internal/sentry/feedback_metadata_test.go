package sentry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFeedbackMetadataPrefersExplicitTagsAndUsesCompatibleFallbacks(t *testing.T) {
	cases := []struct {
		name, event, kind, taskType, datasetID string
	}{
		{
			"explicit tags win",
			`{"eventID":"e","contexts":{"feedback":{"message":"问题描述","feedback_type":"suggestion"},"feedback_details":{"task_type":"Marker","dataset_id":"details-dataset"},"annotation_task":{"taskType":"Lane","datasetId":"scope-dataset"}},"tags":[{"key":"feedback_type","value":"data_issue"},{"key":"task_type","value":"OD_4D"},{"key":"dataset_id","value":"tag-dataset"}]}`,
			"data_issue", "OD_4D", "tag-dataset",
		},
		{
			"feedback-specific context fallback",
			`{"eventID":"e","contexts":{"feedback":{"message":"问题描述","feedback_type":"platform_bug"},"feedback_details":{"task_type":"Marker","dataset_id":"details-dataset"},"annotation_task":{"taskType":"Lane","datasetId":"scope-dataset"}}}`,
			"platform_bug", "Marker", "details-dataset",
		},
		{
			"legacy task context fallback",
			`{"eventID":"e","contexts":{"feedback":{"message":"问题描述","type":"suggestion"},"annotation_task":{"taskType":"Lane","datasetId":"scope-dataset"}}}`,
			"suggestion", "Lane", "scope-dataset",
		},
		{
			"route category is not a task type",
			`{"eventID":"e","contexts":{"feedback":{"message":"问题描述","type":"feedback"}},"tags":[{"key":"feedback_type","value":"platform_bug"},{"key":"feedback_category","value":"OD"},{"key":"category","value":"od"}]}`,
			"platform_bug", "", "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/events/oldest/") {
					_, _ = io.WriteString(w, tc.event)
					return
				}
				if r.URL.Path != "/api/0/organizations/acme/issues/42/" {
					t.Errorf("unexpected request %s", r.URL.Path)
				}
				_, _ = io.WriteString(w, `{"id":"42","issueCategory":"feedback","project":{"id":"5","slug":"web"}}`)
			}))
			defer server.Close()
			client, err := New(server.URL, "test-only-token")
			if err != nil {
				t.Fatal(err)
			}
			feedback, err := client.GetFeedback(context.Background(), "acme", "42", "")
			if err != nil {
				t.Fatal(err)
			}
			if feedback.Kind != tc.kind || feedback.TaskType != tc.taskType || feedback.DatasetID != tc.datasetID || feedback.Message != "问题描述" {
				t.Errorf("unexpected feedback metadata: %+v", feedback)
			}
		})
	}
}
