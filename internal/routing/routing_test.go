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
		CategoryOwners: map[string]string{
			"data_issue": strings.Repeat("6", 24),
			"suggestion": strings.Repeat("7", 24),
		},
		DefaultPlatformExecutorID: strings.Repeat("8", 24),
		Owners:                    []ownerConfig{{Name: "Owner", ExecutorID: strings.Repeat("9", 24), TaskTypes: []string{"OD"}}},
		SuggestionOwners:          []ownerConfig{{Name: "Suggestion owner", ExecutorID: strings.Repeat("a", 24), TaskTypes: []string{"Lidar_seg"}}},
		EnvironmentTags:           map[string]string{"TC": strings.Repeat("b", 24), "HS": strings.Repeat("c", 24)},
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
		wantOwner                              string
		review                                 bool
	}{
		{"platform", "platform_bug", "OD", "platform_bug", config.Owners[0].ExecutorID, false},
		{"normalized case and whitespace", " PLATFORM_BUG ", " od ", "platform_bug", config.Owners[0].ExecutorID, false},
		{"data ignores platform owner", "data_issue", "OD", "data_issue", config.CategoryOwners["data_issue"], false},
		{"data ignores suggestion owner", "data_issue", "Lidar_seg", "data_issue", config.CategoryOwners["data_issue"], false},
		{"data missing task", "data_issue", "", "data_issue", config.CategoryOwners["data_issue"], false},
		{"suggestion uses separate task owner", "suggestion", "Lidar_seg", "suggestion", config.SuggestionOwners[0].ExecutorID, false},
		{"suggestion normalizes task and category", " SUGGESTION ", " lidar_SEG ", "suggestion", config.SuggestionOwners[0].ExecutorID, false},
		{"suggestion ignores platform owner", "suggestion", "OD", "suggestion", config.CategoryOwners["suggestion"], false},
		{"suggestion unconfigured task", "suggestion", "PD_label", "suggestion", config.CategoryOwners["suggestion"], false},
		{"suggestion missing task", "suggestion", "", "suggestion", config.CategoryOwners["suggestion"], false},
		{"other does not assign", "other", "OD", "other", "", false},
		{"legacy ambiguity", "feature_gap", "Lidar_seg", "other", "", true},
		{"unknown category", "new_category", "OD", "other", "", true},
		{"missing category", "", "OD", "other", "", true},
		{"legacy bug", "bug", "OD", "platform_bug", config.Owners[0].ExecutorID, false},
		{"legacy defect", "defect", "OD", "platform_bug", config.Owners[0].ExecutorID, false},
		{"legacy feature", "feature", "Lidar_seg", "suggestion", config.SuggestionOwners[0].ExecutorID, false},
		{"legacy request", "request", "OD", "suggestion", config.CategoryOwners["suggestion"], false},
		{"unconfigured platform task", "platform_bug", "PD_label", "platform_bug", config.DefaultPlatformExecutorID, false},
		{"platform does not reuse suggestion owner", "platform_bug", "Lidar_seg", "platform_bug", config.DefaultPlatformExecutorID, false},
		{"missing platform task", "platform_bug", "", "platform_bug", config.DefaultPlatformExecutorID, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rules.Resolve(tc.feedbackType, tc.taskType, "")
			if got.FeedbackType != tc.wantType || got.TagID != config.FeedbackTags[tc.wantType] {
				t.Errorf("category/tag = %q/%q", got.FeedbackType, got.TagID)
			}
			if got.ExecutorID != tc.wantOwner || (got.ReviewReason != "") != tc.review {
				t.Errorf("owner/review = %q/%q, want %q/review=%t", got.ExecutorID, got.ReviewReason, tc.wantOwner, tc.review)
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
		{"unknown fixed category", func(c *fileConfig) { c.CategoryOwners["unknown"] = strings.Repeat("8", 24) }},
		{"platform cannot override task owners", func(c *fileConfig) { c.CategoryOwners["platform_bug"] = strings.Repeat("8", 24) }},
		{"other remains unassigned", func(c *fileConfig) { c.CategoryOwners["other"] = strings.Repeat("8", 24) }},
		{"empty fixed owner", func(c *fileConfig) { c.CategoryOwners["data_issue"] = "" }},
		{"invalid fixed owner", func(c *fileConfig) { c.CategoryOwners["data_issue"] = "not-an-id" }},
		{"multiple fixed owners", func(c *fileConfig) { c.CategoryOwners["data_issue"] += "," + strings.Repeat("8", 24) }},
		{"missing platform fallback", func(c *fileConfig) { c.DefaultPlatformExecutorID = "" }},
		{"invalid platform fallback", func(c *fileConfig) { c.DefaultPlatformExecutorID = "not-an-id" }},
		{"multiple platform fallbacks", func(c *fileConfig) { c.DefaultPlatformExecutorID += "," + strings.Repeat("8", 24) }},
		{"empty cloud server", func(c *fileConfig) { c.EnvironmentTags[" "] = strings.Repeat("b", 24) }},
		{"empty cloud tag", func(c *fileConfig) { c.EnvironmentTags["TC"] = "" }},
		{"invalid cloud tag", func(c *fileConfig) { c.EnvironmentTags["TC"] = "not-an-id" }},
		{"multiple cloud tags", func(c *fileConfig) { c.EnvironmentTags["TC"] += "," + strings.Repeat("c", 24) }},
		{"duplicate normalized cloud server", func(c *fileConfig) { c.EnvironmentTags[" tc "] = strings.Repeat("c", 24) }},
		{"deployment environment is not a cloud server", func(c *fileConfig) { c.EnvironmentTags["production"] = strings.Repeat("c", 24) }},
		{"cloud name is not a server code", func(c *fileConfig) { c.EnvironmentTags["tencent"] = strings.Repeat("b", 24) }},
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
		{"empty suggestion owner name", func(c *fileConfig) { c.SuggestionOwners[0].Name = " " }},
		{"empty suggestion owner ID", func(c *fileConfig) { c.SuggestionOwners[0].ExecutorID = "" }},
		{"invalid suggestion owner ID", func(c *fileConfig) { c.SuggestionOwners[0].ExecutorID = "not-an-id" }},
		{"multiple suggestion owner IDs", func(c *fileConfig) { c.SuggestionOwners[0].ExecutorID += "," + strings.Repeat("8", 24) }},
		{"empty suggestion task list", func(c *fileConfig) { c.SuggestionOwners[0].TaskTypes = nil }},
		{"empty suggestion task", func(c *fileConfig) { c.SuggestionOwners[0].TaskTypes = []string{" "} }},
		{"duplicate suggestion task in group", func(c *fileConfig) { c.SuggestionOwners[0].TaskTypes = []string{"Lidar_seg", " lidar_SEG "} }},
		{"duplicate suggestion task across groups", func(c *fileConfig) {
			c.SuggestionOwners = append(c.SuggestionOwners, ownerConfig{Name: "Another", ExecutorID: strings.Repeat("8", 24), TaskTypes: []string{" LIDAR_SEG "}})
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
		{"PD_label", "6784ad2f4981b4ff9f939fe3"},
		{"Tagging", "6784ad2f4981b4ff9f939fe3"},
		{"TSR_AS", "6784ad2f4981b4ff9f939fe3"},
		{"OCC", "6784ad2f4981b4ff9f939fe3"},
		{"", "6784ad2f4981b4ff9f939fe3"},
	}
	for _, tc := range cases {
		if got := rules.Resolve("platform_bug", tc.taskType, ""); got.ExecutorID != tc.executorID || got.ReviewReason != "" {
			t.Errorf("owner for %s = %s (%s), want %s", tc.taskType, got.ExecutorID, got.ReviewReason, tc.executorID)
		}
	}
	for _, kind := range []string{"other", "feature_gap"} {
		if got := rules.Resolve(kind, "OD", ""); got.ExecutorID != "" {
			t.Errorf("non-platform feedback %s unexpectedly has an owner", kind)
		}
	}
	for _, taskType := range []string{"OD", "Lidar_seg", "PD_label", ""} {
		if got := rules.Resolve("data_issue", taskType, ""); got.ExecutorID != "68707b9783c2517553ea6203" || got.ReviewReason != "" {
			t.Errorf("data owner for %s = %q (%s)", taskType, got.ExecutorID, got.ReviewReason)
		}
	}
}

func TestConfigurationWithoutFixedOwnersDoesNotUsePlatformOwners(t *testing.T) {
	config := validConfig()
	config.CategoryOwners = nil
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := Load(writeConfig(t, encoded))
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"data_issue", "suggestion"} {
		got := rules.Resolve(kind, "OD", "")
		if got.ExecutorID != "" || got.ReviewReason == "" {
			t.Errorf("missing fixed owner must not fall back to a platform owner: %+v", got)
		}
	}
}

func TestDeploymentSuggestionOwnersCoverEveryApprovedTaskType(t *testing.T) {
	rules, err := Load(filepath.Join("..", "..", "config", "feedback-routing.json"))
	if err != nil {
		t.Fatal(err)
	}
	groups := []struct {
		owner     string
		taskTypes []string
	}{
		{
			"688b39a65ece6d764f3cac6d",
			[]string{"Lidar_seg", "Lidar_seg_4D", "Driving_Lidar_seg", "TLR_4D"},
		},
		{
			"6a277b7030ff5045c8d8bfd8",
			[]string{
				"PSD", "PSD_3D", "Freespace", "Freespace_3D", "Road_3D", "Driving_Lane", "Lane", "Marker", "Road_Topo", "HD_map",
				"2D_seg", "CLF_2D", "PD_check", "Lane_check", "TSR", "TLR", "TLR_E2E", "LM_SRL", "Dst", "Common_2D",
				"VLM_Marker", "VLM_Lane", "VLM_2D", "VLM_PSD", "VLM_Text", "VLM_Text_sequence", "VLA", "VLA_sequence", "Lane_map", "SD_map",
				"OD", "OD_4D", "OD_correct", "OD_check", "Sim_od", "Sim_od_sod", "Occ_check", "Driving_Occ_check",
			},
		},
	}
	if len(rules.suggestionOwners) != 42 {
		t.Errorf("suggestion task types = %d, want 42", len(rules.suggestionOwners))
	}
	for _, group := range groups {
		for _, taskType := range group.taskTypes {
			got := rules.Resolve("suggestion", " "+strings.ToUpper(taskType)+" ", "")
			if got.ExecutorID != group.owner || got.ReviewReason != "" {
				t.Errorf("suggestion owner for %s = %s (%s), want %s", taskType, got.ExecutorID, got.ReviewReason, group.owner)
			}
		}
	}
	for _, taskType := range []string{"PD_label", "Tagging", "TSR_AS", ""} {
		got := rules.Resolve("suggestion", taskType, "")
		if got.ExecutorID != "6a277b7030ff5045c8d8bfd8" || got.ReviewReason != "" {
			t.Errorf("suggestion fallback for %s = %s (%s)", taskType, got.ExecutorID, got.ReviewReason)
		}
	}
}

func TestCloudTagIsIndependentOfCategoryAndOwner(t *testing.T) {
	rules, err := compile(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"platform_bug", "data_issue", "suggestion", "other", "feature_gap", "future_type", ""} {
		withoutCloud := rules.Resolve(kind, "OD", "")
		for _, cloudServer := range []string{"TC", " tc ", " Tc "} {
			got := rules.Resolve(kind, "OD", cloudServer)
			if got.EnvironmentTagID != strings.Repeat("b", 24) {
				t.Errorf("cloud tag for %s/%s = %s", kind, cloudServer, got.EnvironmentTagID)
			}
			got.EnvironmentTagID = ""
			if got != withoutCloud {
				t.Errorf("cloud changed category, owner, or review: got %+v, want %+v", got, withoutCloud)
			}
		}
		for _, cloudServer := range []string{"", " ", "unknown", "production", "test", "pre", "staging", "腾讯", "腾讯云", "tencent", "tencentcloud", "火山", "火山云", "volcengine"} {
			if got := rules.Resolve(kind, "OD", cloudServer); got.EnvironmentTagID != "" {
				t.Errorf("non-server value %q got cloud tag %s", cloudServer, got.EnvironmentTagID)
			}
		}
	}
}

func TestDeploymentCloudServerTags(t *testing.T) {
	rules, err := Load(filepath.Join("..", "..", "config", "feedback-routing.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range []struct {
		tagID   string
		aliases []string
	}{
		{"6afaaa6ba182b75dece7c837", []string{"TC", "tc"}},
		{"6afaaa7a77889592b04252ab", []string{"HS", "hs"}},
	} {
		for _, alias := range group.aliases {
			got := rules.Resolve("other", "", " "+alias+" ")
			if got.EnvironmentTagID != group.tagID {
				t.Errorf("tag for %s = %s, want %s", alias, got.EnvironmentTagID, group.tagID)
			}
		}
	}
}

func TestSuggestionTaskOwnerDoesNotRequireFixedFallback(t *testing.T) {
	config := validConfig()
	config.CategoryOwners = nil
	config.EnvironmentTags = nil
	rules, err := compile(config)
	if err != nil {
		t.Fatal(err)
	}
	got := rules.Resolve("suggestion", "Lidar_seg", "TC")
	if got.ExecutorID != config.SuggestionOwners[0].ExecutorID || got.ReviewReason != "" || got.EnvironmentTagID != "" {
		t.Errorf("suggestion task owner with optional config omitted = %+v", got)
	}
}

func TestOptionalSuggestionMappingAndNormalizedConfiguration(t *testing.T) {
	config := validConfig()
	config.SuggestionOwners = nil
	config.DefaultPlatformExecutorID = " " + config.DefaultPlatformExecutorID + "\t"
	config.EnvironmentTags = map[string]string{" tc ": " " + strings.Repeat("b", 24) + "\t"}
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := Load(writeConfig(t, encoded))
	if err != nil {
		t.Fatal(err)
	}
	got := rules.Resolve("suggestion", "Lidar_seg", "TC")
	if got.ExecutorID != config.CategoryOwners["suggestion"] || got.EnvironmentTagID != strings.Repeat("b", 24) || got.ReviewReason != "" {
		t.Errorf("optional suggestion mapping with normalized cloud server config = %+v", got)
	}
	got = rules.Resolve("platform_bug", "unconfigured", "")
	if got.ExecutorID != strings.Repeat("8", 24) || got.ReviewReason != "" {
		t.Errorf("normalized platform fallback = %+v", got)
	}
}
