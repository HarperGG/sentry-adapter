package worker

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFeedbackSummaryCleansOnlyDescription(t *testing.T) {
	cases := []struct {
		name, message, want string
	}{
		{"wrapped description", "[反馈类型] 平台异常\n\n[联系方式] user@example.com\n\n[问题描述]\n保存失败", "保存失败"},
		{"CRLF", "[反馈类型] 平台异常\r\n\r\n[问题描述]\r\n保存失败\r\n再次操作", "保存失败 再次操作"},
		{"legacy plain text", "保存失败\n\t再次操作", "保存失败 再次操作"},
		{"Markdown image", "保存失败 ![不应出现的图片说明](https://example.invalid/a.png)", "保存失败"},
		{"nested image URL", "保存失败 ![截图](https://example.invalid/a_(1).png)", "保存失败"},
		{"reference image", "保存失败 ![截图][image]\n[image]: https://example.invalid/a.png", "保存失败"},
		{"HTML image", `保存失败 <img src="https://example.invalid/a.png" alt="a > b">`, "保存失败"},
		{"HTML entities", "保存失败 &lt;img src=&quot;https://example.invalid/a.png&quot;&gt;", "保存失败"},
		{"Markdown link", "查看[操作说明](https://example.invalid/help)", "查看操作说明"},
		{"HTML link", `查看 <a href="https://example.invalid/help">操作说明</a>`, "查看 操作说明"},
		{"linked image", "[![截图](https://example.invalid/a.png)](https://example.invalid/detail)", "问题详情见反馈"},
		{"bare URL", "保存失败 https://example.invalid/a.png，刷新无效", "保存失败 ，刷新无效"},
		{"resource schemes", "保存失败 blob:https://example.invalid/id data:image/png;base64,AAAB", "保存失败"},
		{"protocol relative", "保存失败 //example.invalid/a.png", "保存失败"},
		{"empty description", "[反馈类型] 平台异常\n\n[问题描述]\n", "问题详情见反馈"},
		{"images only", "![截图](/image.png)", "问题详情见反馈"},
		{"plain comparison", "数值 < 5 时保存失败", "数值 < 5 时保存失败"},
		{"plain brackets", "按[保存]后无响应", "按[保存]后无响应"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := feedbackSummary(tc.message); got != tc.want {
				t.Errorf("feedbackSummary() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTitlePreservesProblemAndRuneLimits(t *testing.T) {
	message := strings.Repeat("保存失败🚗", 30)
	got := title(strings.Repeat("任务", 60), "dataset-123", message)
	if !utf8.ValidString(got) || len([]rune(got)) != maxTitleRunes {
		t.Errorf("title must contain %d valid Unicode runes: %q", maxTitleRunes, got)
	}
	wantSuffix := "】 " + truncateRunes(message, maxSummaryRunes)
	if !strings.HasSuffix(got, wantSuffix) {
		t.Errorf("long metadata hid the problem summary: %q", got)
	}
}

func TestTitleUsesFallbackMetadataAndNormalizesWhitespace(t *testing.T) {
	if got := title("", "", "![截图](/image.png)"); got != "【未知任务类型: 未知数据集】 问题详情见反馈" {
		t.Errorf("missing metadata title = %q", got)
	}
	if got := title(" OD_4D\n", " dataset-1\t", "保存\n失败"); got != "【OD_4D: dataset-1】 保存 失败" {
		t.Errorf("whitespace title = %q", got)
	}
}

func TestSummaryHandlesLargeUnclosedMarkdown(t *testing.T) {
	message := strings.Repeat("[x](", 50_000)
	if got := feedbackSummary(message); got != truncateRunes(message, maxSummaryRunes) {
		t.Errorf("malformed Markdown must retain its text within the summary limit: %q", got)
	}
}
