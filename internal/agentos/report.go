package agentos

import (
	"encoding/json"
	"errors"
	"strings"
)

// Report holds the fields used for delivery. Analyze returns the original JSON so
// additional fields, including event evidence and missing context, can be persisted.
type Report struct {
	AnalysisStatus  string `json:"analysis_status"`
	IssueKey        string `json:"issue_key"`
	FeedbackSummary struct {
		Description string `json:"description"`
	} `json:"feedback_summary"`
	RootCause struct {
		Conclusion      string `json:"conclusion"`
		Confidence      string `json:"confidence"`
		Category        string `json:"category"`
		ConfidenceScore *int   `json:"confidence_score"`
	} `json:"root_cause"`
	Evidence []struct {
		Observation string `json:"observation"`
		Reference   string `json:"reference"`
	} `json:"evidence"`
	Recommendations struct {
		VerificationSteps []string `json:"verification_steps"`
	} `json:"recommendations"`
}

func ParseReport(raw json.RawMessage) (Report, error) {
	var r Report
	if err := json.Unmarshal(raw, &r); err != nil {
		return Report{}, errors.New("AgentOS report is not a valid JSON report")
	}
	switch r.AnalysisStatus {
	case "分析完成", "部分完成":
		if strings.TrimSpace(r.FeedbackSummary.Description) == "" || strings.TrimSpace(r.RootCause.Conclusion) == "" || strings.TrimSpace(r.RootCause.Category) == "" {
			return Report{}, errors.New("AgentOS report lacks a summary, conclusion or category")
		}
		switch r.RootCause.Confidence {
		case "低", "中", "高":
			score := r.RootCause.ConfidenceScore
			if score == nil || *score < 0 || *score > 100 {
				return Report{}, errors.New("AgentOS report has invalid evidence confidence")
			}
		case "未知":
			if r.RootCause.ConfidenceScore != nil {
				return Report{}, errors.New("AgentOS report includes a score for unknown confidence")
			}
		default:
			return Report{}, errors.New("AgentOS report lacks a confidence level")
		}
	case "分析失败", "信息不足":
	default:
		return Report{}, errors.New("invalid AgentOS analysis status")
	}
	return r, nil
}

func (r Report) Notifiable() bool {
	if r.AnalysisStatus != "分析完成" && r.AnalysisStatus != "部分完成" {
		return false
	}
	for _, e := range r.Evidence {
		if strings.TrimSpace(e.Reference) != "" && strings.TrimSpace(e.Observation) != "" {
			return true
		}
	}
	return false
}
