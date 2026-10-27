package routing

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validConfig() fileConfig {
	return fileConfig{
		FeedbackTags: map[string]string{
			"platform_bug": strings.Repeat("1", 24),
			"data_issue":   strings.Repeat("2", 24),
			"suggestion":   strings.Repeat("3", 24),
			"other":        strings.Repeat("4", 24),
		},
		Owners: []ownerConfig{{Name: "Owner", ExecutorID: strings.Repeat("9", 24), TaskTypes: []string{"OD"}}},
	}
}

func writeConfig(t *testing.T, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "routing.json")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveCategoryAndSingleOwner(t *testing.T) {
	config := validConfig()
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := Load(writeConfig(t, encoded))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, feedbackType, taskType, wantType string
		assigned, review                       bool
	}{
		{"platform", "platform_bug", "OD", "platform_bug", true, false},
		{"normalized case and whitespace", " PLATFORM_BUG ", " od ", "platform_bug", true, false},
		{"data does not assign", "data_issue", "OD", "data_issue", false, false},
		{"suggestion does not assign", "suggestion", "OD", "suggestion", false, false},
		{"other does not assign", "other", "OD", "other", false, false},
		{"legacy ambiguity", "feature_gap", "OD", "other", false, true},
		{"unknown category", "new_category", "OD", "other", false, true},
		{"missing category", "", "OD", "other", false, true},
		{"legacy bug", "bug", "OD", "platform_bug", true, false},
		{"legacy defect", "defect", "OD", "platform_bug", true, false},
		{"legacy feature", "feature", "OD", "suggestion", false, false},
		{"legacy request", "request", "OD", "suggestion", false, false},
		{"unconfigured task", "platform_bug", "PD_label", "platform_bug", false, true},
		{"missing task", "platform_bug", "", "platform_bug", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rules.Resolve(tc.feedbackType, tc.taskType)
			if got.FeedbackType != tc.wantType || got.TagID != config.FeedbackTags[tc.wantType] {
				t.Errorf("category/tag = %q/%q", got.FeedbackType, got.TagID)
			}
			wantOwner := ""
			if tc.assigned {
				wantOwner = config.Owners[0].ExecutorID
			}
			if got.ExecutorID != wantOwner || (got.ReviewReason != "") != tc.review {
				t.Errorf("owner/review = %q/%q, want %q/review=%t", got.ExecutorID, got.ReviewReason, wantOwner, tc.review)
			}
		})
	}
}

func TestLoadRejectsInvalidRouting(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*fileConfig)
	}{
		{"missing category", func(c *fileConfig) { delete(c.FeedbackTags, "data_issue") }},
		{"unknown category", func(c *fileConfig) { c.FeedbackTags["feature_gap"] = strings.Repeat("5", 24) }},
		{"wrong category", func(c *fileConfig) {
			delete(c.FeedbackTags, "other")
			c.FeedbackTags["unknown"] = strings.Repeat("5", 24)
		}},
		{"empty tag", func(c *fileConfig) { c.FeedbackTags["other"] = "" }},
		{"invalid tag", func(c *fileConfig) { c.FeedbackTags["other"] = "not-an-id" }},
		{"duplicate tag", func(c *fileConfig) { c.FeedbackTags["other"] = c.FeedbackTags["data_issue"] }},
		{"empty owner name", func(c *fileConfig) { c.Owners[0].Name = " " }},
		{"empty owner ID", func(c *fileConfig) { c.Owners[0].ExecutorID = "" }},
		{"multiple owner IDs", func(c *fileConfig) {
			c.Owners[0].ExecutorID += "," + strings.Repeat("8", 24)
		}},
		{"empty task list", func(c *fileConfig) { c.Owners[0].TaskTypes = nil }},
		{"empty task type", func(c *fileConfig) { c.Owners[0].TaskTypes = []string{" "} }},
		{"duplicate in group", func(c *fileConfig) { c.Owners[0].TaskTypes = []string{"OD", " od "} }},
		{"duplicate across groups", func(c *fileConfig) {
			c.Owners = append(c.Owners, ownerConfig{Name: "Another", ExecutorID: strings.Repeat("8", 24), TaskTypes: []string{"od"}})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config := validConfig()
			tc.mutate(&config)
			encoded, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Load(writeConfig(t, encoded)); err == nil {
				t.Fatal("invalid routing configuration was accepted")
			}
		})
	}
}

func TestLoadRejectsInvalidJSON(t *testing.T) {
	encoded, err := json.Marshal(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{
		`{"unknown": true}`,
		`{"feedbackTags":{},"owners":[{"name":"Owner","executorId":["a","b"]}]}`,
		string(encoded) + ` {}`,
		string(encoded) + ` garbage`,
		`null`,
		`[]`,
	} {
		if _, err := Load(writeConfig(t, []byte(content))); err == nil {
			t.Error("invalid JSON was accepted")
		}
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("missing routing file was accepted")
	}
}

func TestDeploymentConfigContainsApprovedOwners(t *testing.T) {
	rules, err := Load(filepath.Join("..", "..", "config", "feedback-routing.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rules.owners) != 42 {
		t.Errorf("configured task types = %d, want 42", len(rules.owners))
	}
	cases := []struct{ taskType, executorID string }{
		{"Lidar_seg", "68e8d3202e316da74b8189cc"},
		{"PSD", "6889c08f7c2784422c96ae0f"},
		{"vlm_text_sequence", "69ce485f652bf1ac6daa9b05"},
		{"Lane_map", "69ce485f652bf1ac6daa9b05"},
		{"OD_check", "67c53de0c45defb1e0693426"},
		{"TLR_4D", "67c53de0c45defb1e0693426"},
		{"PD_label", ""},
		{"Tagging", ""},
		{"TSR_AS", ""},
		{"OCC", ""},
	}
	for _, tc := range cases {
		if got := rules.Resolve("platform_bug", tc.taskType); got.ExecutorID != tc.executorID {
			t.Errorf("owner for %s = %s, want %s", tc.taskType, got.ExecutorID, tc.executorID)
		}
	}
	for _, kind := range []string{"data_issue", "suggestion", "other", "feature_gap"} {
		if got := rules.Resolve(kind, "OD"); got.ExecutorID != "" {
			t.Errorf("non-platform feedback %s unexpectedly has an owner", kind)
		}
	}
}
