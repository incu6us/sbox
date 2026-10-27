package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

const (
	_maxToolCalls  = 200 // kept per transcript
	_maxSummaryLen = 160
	_maxFinalText  = 300
)

// agentTranscript is what an agent said it did, from Claude Code's
// --output-format stream-json lines.
type agentTranscript struct {
	Model     string     `json:"model"`
	Status    string     `json:"status"` // the result line's subtype: success, error_max_turns, ...
	IsError   bool       `json:"is_error"`
	Turns     int        `json:"turns"`
	ToolCalls []toolCall `json:"tool_calls"`
	FinalText string     `json:"final_text"`
}

// toolCall is one tool the agent used, with a one-line summary of its input.
type toolCall struct {
	ID      string `json:"-"`
	Tool    string `json:"tool"`
	Summary string `json:"summary"`
	Failed  bool   `json:"failed"`
}

// streamLine is the part of a stream-json line the parser reads.
type streamLine struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Model   string `json:"model"`
	Message struct {
		Content []struct {
			Type      string          `json:"type"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
			IsError   bool            `json:"is_error"`
		} `json:"content"`
	} `json:"message"`
	IsError  bool   `json:"is_error"`
	NumTurns int    `json:"num_turns"`
	Result   string `json:"result"`
}

// parseTranscript reads stream-json lines. Lines that aren't JSON, or are of
// a type it doesn't know, are skipped: the transcript comes from inside the
// guest, so it informs the report but never decides the verdict.
func parseTranscript(r io.Reader) agentTranscript {
	var t agentTranscript
	byID := make(map[string]int)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		var line streamLine
		if json.Unmarshal(sc.Bytes(), &line) != nil {
			continue
		}
		switch line.Type {
		case "system":
			if line.Subtype == "init" && line.Model != "" {
				t.Model = line.Model
			}
		case "assistant":
			for _, c := range line.Message.Content {
				if c.Type != "tool_use" || len(t.ToolCalls) >= _maxToolCalls {
					continue
				}
				byID[c.ID] = len(t.ToolCalls)
				t.ToolCalls = append(t.ToolCalls, toolCall{ID: c.ID, Tool: c.Name, Summary: summarizeInput(c.Name, c.Input)})
			}
		case "user":
			for _, c := range line.Message.Content {
				if i, ok := byID[c.ToolUseID]; ok && c.Type == "tool_result" && c.IsError {
					t.ToolCalls[i].Failed = true
				}
			}
		case "result":
			t.Status, t.IsError, t.Turns = line.Subtype, line.IsError, line.NumTurns
			t.FinalText = truncateText(line.Result, _maxFinalText)
		}
	}
	return t
}

// summarizeInput picks the field that says what a tool call did.
func summarizeInput(tool string, raw json.RawMessage) string {
	var in map[string]any
	if json.Unmarshal(raw, &in) != nil {
		return ""
	}
	for _, key := range []string{"command", "file_path", "notebook_path", "url", "pattern", "skill", "query", "description", "prompt"} {
		if v, ok := in[key].(string); ok && v != "" {
			return truncateText(v, _maxSummaryLen)
		}
	}
	compact, _ := json.Marshal(in) // a decoded map always re-encodes
	return truncateText(string(compact), _maxSummaryLen)
}

func truncateText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// writeTranscript renders the agent section of a report.
func writeTranscript(b *strings.Builder, agent string, t agentTranscript) {
	status := t.Status
	if status == "" {
		status = "no result (stopped early)"
	}
	fmt.Fprintf(b, "     %-10s %s (%s), %d turns, %s\n", "agent", agent, printable(t.Model), t.Turns, printable(status))
	if len(t.ToolCalls) == 0 {
		fmt.Fprintf(b, "     %-10s none\n", "tool calls")
	}
	for i, c := range t.ToolCalls {
		label := ""
		if i == 0 {
			label = "tool calls"
		}
		failed := ""
		if c.Failed {
			failed = " (failed)"
		}
		fmt.Fprintf(b, "     %-10s %-8s %s%s\n", label, printable(c.Tool), printable(c.Summary), failed)
	}
	if t.FinalText != "" {
		fmt.Fprintf(b, "     %-10s %s\n", "said", printable(t.FinalText))
	}
}
