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
	FeedbackType    string `json:"feedback_type"`
	FeedbackSummary struct {
		Description string `json:"description"`
		Impact      string `json:"impact"`
	} `json:"feedback_summary"`
	RootCause struct {
		Conclusion      string `json:"conclusion"`
		Confidence      string `json:"confidence"`
		Category        string `json:"category"`
		ConfidenceScore *int   `json:"confidence_score"`
	} `json:"root_cause"`
	Triage struct {
		SuggestedCategory string `json:"suggested_category"`
		Reason            string `json:"reason"`
	} `json:"triage"`
	Evidence        []Evidence `json:"evidence"`
	Recommendations struct {
		VerificationSteps []string `json:"verification_steps"`
	} `json:"recommendations"`
}

type Evidence struct {
	Observation string `json:"observation"`
	Reference   string `json:"reference"`
}

func ParseReportForType(raw json.RawMessage, feedbackType string) (Report, error) {
	switch feedbackType {
	case "platform_bug", "data_issue", "other":
	default:
		return Report{}, errors.New("unsupported feedback type for analysis")
	}
	var r Report
	if err := json.Unmarshal(raw, &r); err != nil {
		return Report{}, errors.New("AgentOS report is not a valid JSON report")
	}
	if r.FeedbackType != "" && r.FeedbackType != feedbackType {
		return Report{}, errors.New("AgentOS report feedback type differs from the job")
	}
	switch r.AnalysisStatus {
	case "分析完成", "部分完成":
		if err := r.validateFindings(feedbackType); err != nil {
			return Report{}, err
		}
	case "分析失败", "信息不足":
	default:
		return Report{}, errors.New("invalid AgentOS analysis status")
	}
	return r, nil
}

func (r Report) validateFindings(feedbackType string) error {
	if strings.TrimSpace(r.FeedbackSummary.Description) == "" {
		return errors.New("AgentOS report lacks a feedback summary")
	}
	// Persisted reports from the platform-only rollout have no feedback_type.
	if feedbackType == "platform_bug" {
		return r.validateRootCause()
	}
	if r.FeedbackType == "" {
		return errors.New("AgentOS report lacks the requested feedback type")
	}
	if len(r.Recommendations.VerificationSteps) == 0 || strings.TrimSpace(r.Recommendations.VerificationSteps[0]) == "" {
		return errors.New("AgentOS report lacks an actionable next step")
	}
	if feedbackType == "data_issue" {
		if strings.TrimSpace(r.FeedbackSummary.Impact) == "" {
			return errors.New("AgentOS data report lacks the observed or unknown impact")
		}
		return nil
	}
	switch r.Triage.SuggestedCategory {
	case "platform_bug", "data_issue", "suggestion", "other":
	default:
		return errors.New("AgentOS triage report has an invalid suggested category")
	}
	if strings.TrimSpace(r.Triage.Reason) == "" {
		return errors.New("AgentOS triage report lacks a routing reason")
	}
	return nil
}

func (r Report) validateRootCause() error {
	if strings.TrimSpace(r.RootCause.Conclusion) == "" || strings.TrimSpace(r.RootCause.Category) == "" {
		return errors.New("AgentOS report lacks a conclusion or category")
	}
	switch r.RootCause.Confidence {
	case "低", "中", "高":
		score := r.RootCause.ConfidenceScore
		if score == nil || *score < 0 || *score > 100 {
			return errors.New("AgentOS report has invalid evidence confidence")
		}
	case "未知":
		if r.RootCause.ConfidenceScore != nil {
			return errors.New("AgentOS report includes a score for unknown confidence")
		}
	default:
		return errors.New("AgentOS report lacks a confidence level")
	}
	return nil
}

func (r Report) FirstEvidence() (Evidence, bool) {
	for _, e := range r.Evidence {
		if strings.TrimSpace(e.Reference) != "" && strings.TrimSpace(e.Observation) != "" {
			return e, true
		}
	}
	return Evidence{}, false
}

func (r Report) Notifiable() bool {
	_, ok := r.FirstEvidence()
	return ok && (r.AnalysisStatus == "分析完成" || r.AnalysisStatus == "部分完成")
}
