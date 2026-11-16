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

func TestFeedbackCloudServerUsesBusinessURL(t *testing.T) {
	cases := []struct {
		name, event, want string
	}{
		{
			"real feedback URL retains query omitted from URL tag",
			`{"eventID":"e","environment":"production","contexts":{"feedback":{"url":"https://annotation.gwm-adas.com/TSR_AS/?taskId=42&server=TC"}},"tags":[{"key":"url","value":"https://annotation.gwm-adas.com/TSR_AS/"},{"key":"environment","value":"production"}]}`,
			"TC",
		},
		{
			"volcano cloud comes from business server",
			`{"eventID":"e","environment":"staging","contexts":{"feedback":{"url":"https://annotation.gwm-adas.com/Lane/?server=HS"}}}`,
			"HS",
		},
		{
			"server value is decoded trimmed and uppercased",
			`{"eventID":"e","contexts":{"feedback":{"url":"  https://annotation.gwm-adas.com/Lane/?server=%20tc%09  "}}}`,
			"TC",
		},
		{
			"URL tag fallback when context URL is absent",
			`{"eventID":"e","tags":[{"key":"url","value":"http://annotation.example/Lane/?server=hs"}]}`,
			"HS",
		},
		{
			"blank context and first tag URL allow nonempty tag fallback",
			`{"eventID":"e","contexts":{"feedback":{"url":" \t "}},"tags":[{"key":"url","value":" "},{"key":"url","value":"https://annotation.example/?server=TC"}]}`,
			"TC",
		},
		{
			"feedback URL precedes URL tag",
			`{"eventID":"e","contexts":{"feedback":{"url":"https://annotation.example/?server=HS"}},"tags":[{"key":"url","value":"https://annotation.example/?server=TC"}]}`,
			"HS",
		},
		{
			"nonempty context URL without server does not use URL tag",
			`{"eventID":"e","contexts":{"feedback":{"url":"https://annotation.example/"}},"tags":[{"key":"url","value":"https://annotation.example/?server=TC"}]}`,
			"",
		},
		{
			"invalid nonempty context URL does not use URL tag",
			`{"eventID":"e","contexts":{"feedback":{"url":"not a URL"}},"tags":[{"key":"url","value":"https://annotation.example/?server=TC"}]}`,
			"",
		},
		{
			"unknown server is preserved for routing to ignore",
			`{"eventID":"e","contexts":{"feedback":{"url":"https://annotation.example/?server=private"}}}`,
			"PRIVATE",
		},
		{
			"missing source URL does not infer server from environment or other fields",
			`{"eventID":"e","environment":"TC","contexts":{"feedback":{"message":"https://annotation.example/?server=TC","environment":"HS"},"feedback_details":{"url":"https://annotation.example/?server=HS","environment":"HS"},"annotation_task":{"environment":"TC"}},"tags":[{"key":"environment","value":"HS"},{"key":"cloud","value":"TC"}]}`,
			"",
		},
		{
			"empty server is absent",
			`{"eventID":"e","contexts":{"feedback":{"url":"https://annotation.example/?server=%20"}}}`,
			"",
		},
		{
			"relative business URL is invalid",
			`{"eventID":"e","contexts":{"feedback":{"url":"/Lane/?server=TC"}}}`,
			"",
		},
		{
			"non HTTP business URL is invalid",
			`{"eventID":"e","contexts":{"feedback":{"url":"ftp://annotation.example/?server=TC"}}}`,
			"",
		},
		{
			"invalid absolute business URL is ignored",
			`{"eventID":"e","contexts":{"feedback":{"url":"https://[invalid/?server=TC"}}}`,
			"",
		},
		{
			"invalid query encoding is ignored",
			`{"eventID":"e","contexts":{"feedback":{"url":"https://annotation.example/?server=%GG"}}}`,
			"",
		},
		{
			"invalid unrelated query does not partially recover server",
			`{"eventID":"e","contexts":{"feedback":{"url":"https://annotation.example/?server=TC&other=%GG"}}}`,
			"",
		},
		{
			"fragment pseudo query is not a server query",
			`{"eventID":"e","contexts":{"feedback":{"url":"https://annotation.example/#/Lane?server=TC"}}}`,
			"",
		},
		{
			"nested URL query is not a server query",
			`{"eventID":"e","contexts":{"feedback":{"url":"https://annotation.example/?next=https%3A%2F%2Fother.example%2F%3Fserver%3DTC"}}}`,
			"",
		},
		{
			"duplicate different server parameters are ambiguous",
			`{"eventID":"e","contexts":{"feedback":{"url":"https://annotation.example/?server=TC&server=HS"}}}`,
			"",
		},
		{
			"duplicate identical server parameters are ambiguous",
			`{"eventID":"e","contexts":{"feedback":{"url":"https://annotation.example/?server=TC&server=TC"}}}`,
			"",
		},
		{
			"encoded duplicate server key is also ambiguous",
			`{"eventID":"e","contexts":{"feedback":{"url":"https://annotation.example/?server=TC&%73erver=HS"}}}`,
			"",
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
				_, _ = io.WriteString(w, `{"id":"42","issueCategory":"feedback","permalink":"https://sentry.example/issues/42/?server=TC","project":{"id":"5","slug":"tencent"}}`)
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
			if feedback.CloudServer != tc.want {
				t.Errorf("cloud server = %q, want %q", feedback.CloudServer, tc.want)
			}
		})
	}
}
