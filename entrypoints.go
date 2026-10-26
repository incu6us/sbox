package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// entryPoint is something a skill or plugin runs without the agent deciding
// to: a hook, an MCP server, an install script.
type entryPoint struct {
	Kind    string // hook, mcp, npm, setup.py
	Name    string // event, server or script name
	Source  string // file it was declared in, relative to the skill
	Command string // shell command, run from the skill's root
	Stdin   string // fed to the command
}

var (
	// _configFiles may declare hooks or MCP servers.
	_configFiles = map[string]bool{
		"hooks.json": true, "settings.json": true, "settings.local.json": true,
		"plugin.json": true, ".mcp.json": true, "mcp.json": true,
	}
	_npmHooks = []string{"preinstall", "install", "postinstall", "prepare"}
)

// findEntryPoints lists a skill's automatic entry points: hook commands
// and MCP servers from its config files, and install scripts from its root
// package.json and setup.py. Dependencies and git internals are skipped.
func findEntryPoints(dir string) ([]entryPoint, error) {
	var eps []entryPoint
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "node_modules" || d.Name() == ".git") {
			return filepath.SkipDir
		}
		if !d.Type().IsRegular() || !_configFiles[d.Name()] {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		eps = append(eps, configEntryPoints(filepath.ToSlash(rel), data)...)
		return nil
	})
	if err != nil {
		return nil, err
	}

	if data, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil {
		var pkg struct{ Scripts map[string]string }
		if json.Unmarshal(data, &pkg) == nil {
			for _, hook := range _npmHooks {
				if _, ok := pkg.Scripts[hook]; ok {
					eps = append(eps, entryPoint{Kind: "npm", Name: hook, Source: "package.json", Command: "npm run-script " + hook})
				}
			}
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "setup.py")); err == nil {
		eps = append(eps, entryPoint{Kind: "setup.py", Name: "egg_info", Source: "setup.py", Command: "python3 setup.py egg_info"})
	}
	return eps, nil
}

// configEntryPoints walks a JSON config for hook commands (under "hooks",
// keyed by event: Claude Code's and Cursor's layouts both) and stdio MCP
// servers (under "mcpServers").
func configEntryPoints(rel string, data []byte) []entryPoint {
	var doc any
	if json.Unmarshal(data, &doc) != nil {
		return nil
	}
	var (
		eps  []entryPoint
		walk func(v any, keys []string)
	)
	walk = func(v any, keys []string) {
		switch v := v.(type) {
		case map[string]any:
			if ep, ok := configEntry(rel, v, keys); ok {
				eps = append(eps, ep)
			}
			for _, k := range slices.Sorted(maps.Keys(v)) {
				walk(v[k], append(slices.Clip(keys), k))
			}
		case []any:
			for _, child := range v {
				walk(child, keys)
			}
		}
	}
	walk(doc, nil)
	return eps
}

func configEntry(rel string, entry map[string]any, keys []string) (entryPoint, bool) {
	cmd, ok := entry["command"].(string)
	if !ok || cmd == "" {
		return entryPoint{}, false
	}
	if event, ok := keyAfter(keys, "hooks"); ok {
		return entryPoint{Kind: "hook", Name: event, Source: rel, Command: cmd, Stdin: hookInput(event)}, true
	}
	if name, ok := keyAfter(keys, "mcpServers"); ok {
		var args []string
		if list, ok := entry["args"].([]any); ok {
			for _, a := range list {
				if s, ok := a.(string); ok {
					args = append(args, s)
				}
			}
		}
		full := shellJoin(append([]string{cmd}, args...))
		return entryPoint{Kind: "mcp", Name: name, Source: rel, Command: full, Stdin: mcpHandshake()}, true
	}
	return entryPoint{}, false
}

// hookInput is a realistic event payload for a hook's stdin, so the hook
// takes its normal path instead of failing on empty input.
func hookInput(event string) string {
	in := map[string]any{
		"session_id":      "sbox",
		"transcript_path": "/home/runner/.claude/transcript.jsonl",
		"cwd":             "/home/runner/work",
		"hook_event_name": event,
	}
	switch strings.ToLower(event) {
	case "pretooluse", "posttooluse", "beforeshellexecution", "aftershellexecution":
		in["tool_name"] = "Bash"
		in["tool_input"] = map[string]any{"command": "ls"}
		in["command"] = "ls"
	case "userpromptsubmit", "beforesubmitprompt":
		in["prompt"] = "hello"
	case "sessionstart":
		in["source"] = "startup"
	}
	b, _ := json.Marshal(in) // plain maps always marshal
	return string(b) + "\n"
}

// mcpHandshake is what a client sends first: initialize, then tools/list.
func mcpHandshake() string {
	msgs := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"sbox","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	}
	return strings.Join(msgs, "\n") + "\n"
}

func keyAfter(keys []string, k string) (string, bool) {
	i := slices.Index(keys, k)
	if i < 0 || i+1 >= len(keys) {
		return "", false
	}
	return keys[i+1], true
}

func (e entryPoint) String() string {
	return fmt.Sprintf("%s %s (%s)", e.Kind, e.Name, e.Source)
}
