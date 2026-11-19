package agentos

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const maxStreamSize = 16 << 20

// readReportStream extracts the application response, never Sentry tool outputs.
func readReportStream(body io.Reader, feedbackType string) (json.RawMessage, error) {
	limited := &io.LimitedReader{R: body, N: maxStreamSize + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 32<<10), maxStreamSize)
	var data []string
	var reply strings.Builder
	var event string
	dispatch := func() (bool, error) {
		if len(data) == 0 {
			return false, nil
		}
		switch event {
		case "error":
			return false, errors.New("AgentOS stream reported an error")
		case "", "message", "text", "done":
		default:
			// Tool, thinking and step frames belong to the execution trace.
			return false, nil
		}
		var part struct {
			Type    string `json:"type"`
			Content string `json:"content"`
			Reason  string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(strings.Join(data, "\n")), &part); err != nil {
			return false, errors.New("malformed AgentOS stream event")
		}
		switch part.Type {
		case "text":
			reply.WriteString(part.Content)
		case "error":
			return false, errors.New("AgentOS stream reported an error")
		case "done":
			if part.Reason != "completed" {
				return false, errors.New("AgentOS analysis did not complete")
			}
			return true, nil
		}
		return false, nil
	}
	for scanner.Scan() {
		if limited.N == 0 {
			return nil, errors.New("AgentOS stream exceeds size limit")
		}
		line := scanner.Text()
		switch {
		case line == "":
			done, err := dispatch()
			if err != nil {
				return nil, err
			}
			if done {
				return extractReport(reply.String(), feedbackType)
			}
			event = ""
			data = data[:0]
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(line[len("data:"):], " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read AgentOS stream: %w", err)
	}
	if limited.N <= 0 {
		return nil, errors.New("AgentOS stream exceeds size limit")
	}
	done, err := dispatch()
	if err != nil {
		return nil, err
	}
	if !done {
		return nil, errors.New("AgentOS stream ended before completion")
	}
	return extractReport(reply.String(), feedbackType)
}

func extractReport(reply, feedbackType string) (json.RawMessage, error) {
	const fence = "```"
	s := strings.TrimSpace(strings.ReplaceAll(reply, "\r\n", "\n"))
	if strings.HasPrefix(s, "{") && json.Valid([]byte(s)) {
		if _, err := ParseReportForType([]byte(s), feedbackType); err != nil {
			return nil, err
		}
		return json.RawMessage(s), nil
	}
	lines := strings.Split(s, "\n")
	if strings.TrimSpace(lines[len(lines)-1]) != fence {
		return nil, errors.New("incomplete AgentOS report")
	}
	for i := len(lines) - 2; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == fence {
			break
		}
		if strings.EqualFold(line, fence+"json") {
			raw := json.RawMessage(strings.TrimSpace(strings.Join(lines[i+1:len(lines)-1], "\n")))
			if _, err := ParseReportForType(raw, feedbackType); err != nil {
				return nil, err
			}
			return raw, nil
		}
	}
	return nil, errors.New("AgentOS assistant response has no final JSON report")
}
