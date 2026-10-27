package main

import (
	"os"
	"strings"
	"testing"
)

func TestParseTranscript(t *testing.T) {
	t.Parallel()

	f, err := os.Open("testdata/claude-stream.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	got := parseTranscript(f)

	if got.Model != "claude-haiku-5-5" || got.Status != "success" || got.IsError || got.Turns != 4 {
		t.Errorf("header = %q %q %v %d", got.Model, got.Status, got.IsError, got.Turns)
	}
	want := []toolCall{
		{ID: "t1", Tool: "Skill", Summary: "json-format"},
		{ID: "t2", Tool: "Read", Summary: "/home/runner/work/data.json"},
		{ID: "t3", Tool: "Bash", Summary: "python3 scripts/format.py data.json", Failed: true},
		{ID: "t4", Tool: "TodoWrite", Summary: `{"todos":[{"content":"x","status":"done"}]}`},
	}
	if len(got.ToolCalls) != len(want) {
		t.Fatalf("tool calls = %+v", got.ToolCalls)
	}
	for i := range want {
		if got.ToolCalls[i] != want[i] {
			t.Errorf("tool call %d = %+v, want %+v", i, got.ToolCalls[i], want[i])
		}
	}
	if got.FinalText != "I tried to format data.json, but the script was missing. Nothing was changed." {
		t.Errorf("final text = %q", got.FinalText)
	}
}

func TestParseTranscriptStoppedEarly(t *testing.T) {
	t.Parallel()

	got := parseTranscript(strings.NewReader(`{"type":"system","subtype":"init","model":"m"}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"a","name":"Bash","input":{"command":"sleep 99"}}]}}`))
	var b strings.Builder
	writeTranscript(&b, "claude", got)
	out := b.String()
	for _, want := range []string{"claude (m), 0 turns, no result (stopped early)", "Bash     sleep 99"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}

func TestWriteTranscriptEscapes(t *testing.T) {
	t.Parallel()

	got := parseTranscript(strings.NewReader(
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"a","name":"Bash","input":{"command":"echo \u001b[2J"}}]}}` + "\n" +
			`{"type":"result","subtype":"success","num_turns":1,"result":"done"}`))
	var b strings.Builder
	writeTranscript(&b, "claude", got)
	if strings.Contains(b.String(), "\x1b") {
		t.Errorf("terminal escape reached the report: %q", b.String())
	}
}
