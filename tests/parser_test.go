package tests

import (
	"testing"

	"github.com/jgavinray/gpt-oss-executor/internal/parser"
)

// TestParseGuidedJSON covers Tier 1: structured JSON output from the model.
func TestParseGuidedJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		input      string
		wantNames  []string // expected canonical tool names, in order
		wantNoCall bool     // true when we expect an empty result
	}{
		{
			name: "valid json with tool_calls done=false",
			input: `{
				"reasoning": "I need to search the web",
				"tool_calls": [
					{"name": "web_search", "arguments": {"query": "golang context"}}
				],
				"done": false
			}`,
			wantNames: []string{"web_search"},
		},
		{
			name: "valid json multiple tools done=false",
			input: `{
				"reasoning": "fetch and read",
				"tool_calls": [
					{"name": "web_fetch",  "arguments": {"url": "https://example.com"}},
					{"name": "read_file",  "arguments": {"path": "/tmp/out.txt"}}
				],
				"done": false
			}`,
			wantNames: []string{"web_fetch", "read"},
		},
		{
			name: "done=true with no tool_calls means task complete",
			input: `{
				"reasoning": "all done",
				"tool_calls": [],
				"done": true
			}`,
			wantNoCall: true,
		},
		{
			name: "done=true with tool_calls still returns tools",
			input: `{
				"reasoning": "one last call",
				"tool_calls": [
					{"name": "exec", "arguments": {"command": "ls"}}
				],
				"done": true
			}`,
			wantNames: []string{"exec"},
		},
		{
			name: "json inside code fence",
			input: "Here is the response:\n```json\n" + `{
				"reasoning": "need info",
				"tool_calls": [{"name": "search", "arguments": {"query": "slog"}}],
				"done": false
			}` + "\n```",
			wantNames: []string{"web_search"},
		},
		{
			name:       "malformed json returns no intents",
			input:      `{"reasoning": "broken", "tool_calls": [`,
			wantNoCall: true,
		},
		{
			name: "unknown tool name is skipped",
			input: `{
				"reasoning": "dunno",
				"tool_calls": [{"name": "teleport", "arguments": {}}],
				"done": false
			}`,
			wantNoCall: true,
		},
		{
			name: "duplicate tool names are deduplicated",
			input: `{
				"reasoning": "twice",
				"tool_calls": [
					{"name": "web_search", "arguments": {"query": "a"}},
					{"name": "web_search", "arguments": {"query": "b"}}
				],
				"done": false
			}`,
			wantNames: []string{"web_search"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := parser.New("guided_json", "")
			intents := p.Parse(tc.input)

			if tc.wantNoCall {
				if len(intents) != 0 {
					t.Errorf("expected no intents, got %d: %v", len(intents), intents)
				}
				return
			}

			if len(intents) != len(tc.wantNames) {
				t.Fatalf("expected %d intent(s), got %d: %v", len(tc.wantNames), len(intents), intents)
			}
			for i, want := range tc.wantNames {
				if intents[i].Name != want {
					t.Errorf("intent[%d].Name = %q, want %q", i, intents[i].Name, want)
				}
				if intents[i].Confidence != 1.0 {
					t.Errorf("intent[%d].Confidence = %.2f, want 1.0", i, intents[i].Confidence)
				}
			}
		})
	}
}

// TestParseReAct covers Tier 2: ReAct Action/Action Input line pairs.
func TestParseReAct(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		input      string
		wantNames  []string
		wantNoCall bool
	}{
		{
			name: "single action with json input",
			input: `Thought: I should search for this.
Action: web_search
Action Input: {"query": "Go 1.22 release notes"}`,
			wantNames: []string{"web_search"},
		},
		{
			name: "action with plain string input falls back to input key",
			input: `Action: exec
Action Input: ls -la /tmp`,
			wantNames: []string{"exec"},
		},
		{
			name: "multiple action blocks",
			input: `Action: web_search
Action Input: {"query": "slog structured logging"}
Observation: found results
Action: web_fetch
Action Input: {"url": "https://pkg.go.dev/log/slog"}`,
			wantNames: []string{"web_search", "web_fetch"},
		},
		{
			name: "Action: done stops processing",
			input: `Action: web_search
Action Input: {"query": "golang"}
Action: done`,
			wantNames: []string{"web_search"},
		},
		{
			name: "Action: done as first action returns nothing",
			input: `Thought: I am done.
Action: done`,
			wantNoCall: true,
		},
		{
			name: "alias normalisation via react",
			input: `Action: websearch
Action Input: {"query": "test"}`,
			wantNames: []string{"web_search"},
		},
		{
			name:       "no Action lines returns nothing",
			input:      `Thought: hmm, let me think about this.`,
			wantNoCall: true,
		},
		{
			name: "confidence is 0.9",
			input: `Action: read
Action Input: {"path": "/etc/hosts"}`,
			wantNames: []string{"read"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := parser.New("react", "")
			intents := p.Parse(tc.input)

			if tc.wantNoCall {
				if len(intents) != 0 {
					t.Errorf("expected no intents, got %d: %v", len(intents), intents)
				}
				return
			}

			if len(intents) != len(tc.wantNames) {
				t.Fatalf("expected %d intent(s), got %d: %v", len(tc.wantNames), len(intents), intents)
			}
			for i, want := range tc.wantNames {
				if intents[i].Name != want {
					t.Errorf("intent[%d].Name = %q, want %q", i, intents[i].Name, want)
				}
				if intents[i].Confidence != 0.9 {
					t.Errorf("intent[%d].Confidence = %.2f, want 0.9", i, intents[i].Confidence)
				}
			}
		})
	}
}

// TestParseMarkers covers Tier 3: [TOOL:name|key=val] inline markers.
func TestParseMarkers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		input      string
		wantName   string
		wantArgs   map[string]string
		wantNoCall bool
	}{
		{
			name:     "basic web_search marker",
			input:    `[TOOL:web_search|query=hello world]`,
			wantName: "web_search",
			wantArgs: map[string]string{"query": "hello world"},
		},
		{
			name:     "tolerant of spaces around colon and pipe",
			input:    `[ TOOL : web_search | query = hello ]`,
			wantName: "web_search",
			wantArgs: map[string]string{"query": "hello"},
		},
		{
			name:     "multi-arg marker",
			input:    `[TOOL:web_fetch|url=https://example.com|timeout=30]`,
			wantName: "web_fetch",
			wantArgs: map[string]string{"url": "https://example.com", "timeout": "30"},
		},
		{
			name:     "alias normalised in marker",
			input:    `[TOOL:websearch|query=normalised]`,
			wantName: "web_search",
			wantArgs: map[string]string{"query": "normalised"},
		},
		{
			name:     "exec tool",
			input:    `[TOOL:exec|command=ls -la]`,
			wantName: "exec",
			wantArgs: map[string]string{"command": "ls -la"},
		},
		{
			name:     "case insensitive TOOL keyword",
			input:    `[tool:read|path=/tmp/file.txt]`,
			wantName: "read",
			wantArgs: map[string]string{"path": "/tmp/file.txt"},
		},
		{
			name:       "no markers returns nothing",
			input:      `just some plain text with no markers`,
			wantNoCall: true,
		},
		{
			name:       "unknown tool in marker is skipped",
			input:      `[TOOL:teleporter|dest=mars]`,
			wantNoCall: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := parser.New("markers", "")
			intents := p.Parse(tc.input)

			if tc.wantNoCall {
				if len(intents) != 0 {
					t.Errorf("expected no intents, got %d: %v", len(intents), intents)
				}
				return
			}

			if len(intents) == 0 {
				t.Fatalf("expected at least one intent, got none")
			}
			if intents[0].Name != tc.wantName {
				t.Errorf("Name = %q, want %q", intents[0].Name, tc.wantName)
			}
			for k, want := range tc.wantArgs {
				got, ok := intents[0].Args[k]
				if !ok {
					t.Errorf("Args missing key %q", k)
					continue
				}
				if got != want {
					t.Errorf("Args[%q] = %q, want %q", k, got, want)
				}
			}
			if intents[0].Confidence != 0.85 {
				t.Errorf("Confidence = %.2f, want 0.85", intents[0].Confidence)
			}
		})
	}
}

// TestParseFuzzy covers Tier 4: natural language heuristic matching.
func TestParseFuzzy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		input      string
		wantName   string
		wantArgKey string
		wantArgVal string
		wantNoCall bool
	}{
		{
			name:       "search for phrase triggers web_search",
			input:      "search for Rust async",
			wantName:   "web_search",
			wantArgKey: "query",
			wantArgVal: "Rust async",
		},
		{
			name:       "look up phrase triggers web_search",
			input:      "look up the Go scheduler",
			wantName:   "web_search",
			wantArgKey: "query",
			wantArgVal: "the Go scheduler",
		},
		{
			name:       "fetch URL triggers web_fetch",
			input:      "fetch https://example.com",
			wantName:   "web_fetch",
			wantArgKey: "url",
			wantArgVal: "https://example.com",
		},
		{
			name:       "retrieve page at URL triggers web_fetch",
			input:      "retrieve the page at https://golang.org/doc",
			wantName:   "web_fetch",
			wantArgKey: "url",
			wantArgVal: "https://golang.org/doc",
		},
		{
			name:       "read absolute path triggers read",
			input:      "read the file /etc/hosts",
			wantName:   "read",
			wantArgKey: "path",
			wantArgVal: "/etc/hosts",
		},
		{
			name:       "write to path triggers write",
			input:      "write to /tmp/output.txt",
			wantName:   "write",
			wantArgKey: "path",
			wantArgVal: "/tmp/output.txt",
		},
		{
			name:       "run quoted command triggers exec",
			input:      `run "go test ./..."`,
			wantName:   "exec",
			wantArgKey: "command",
			wantArgVal: "go test ./...",
		},
		{
			name:       "execute quoted command triggers exec",
			input:      `execute "ls -la"`,
			wantName:   "exec",
			wantArgKey: "command",
			wantArgVal: "ls -la",
		},
		{
			name:       "unrecognised text returns nothing",
			input:      "the quick brown fox jumps over the lazy dog",
			wantNoCall: true,
		},
		{
			name:       "confidence is 0.6",
			input:      "search for test confidence",
			wantName:   "web_search",
			wantArgKey: "query",
			wantArgVal: "test confidence",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := parser.New("fuzzy", "")
			intents := p.Parse(tc.input)

			if tc.wantNoCall {
				if len(intents) != 0 {
					t.Errorf("expected no intents, got %d: %v", len(intents), intents)
				}
				return
			}

			if len(intents) == 0 {
				t.Fatalf("expected at least one intent, got none for input: %q", tc.input)
			}
			if intents[0].Name != tc.wantName {
				t.Errorf("Name = %q, want %q", intents[0].Name, tc.wantName)
			}
			got, ok := intents[0].Args[tc.wantArgKey]
			if !ok {
				t.Fatalf("Args missing key %q; got args: %v", tc.wantArgKey, intents[0].Args)
			}
			if got != tc.wantArgVal {
				t.Errorf("Args[%q] = %q, want %q", tc.wantArgKey, got, tc.wantArgVal)
			}
			if intents[0].Confidence != 0.6 {
				t.Errorf("Confidence = %.2f, want 0.6", intents[0].Confidence)
			}
		})
	}
}

// TestNormalizeTool verifies that every documented alias resolves to the
// correct canonical tool name.
func TestNormalizeTool_Aliases(t *testing.T) {
	t.Parallel()

	// Each pair is (alias → expected canonical name).
	cases := []struct {
		alias    string
		wantName string
	}{
		// web_search aliases
		{"web_search", "web_search"},
		{"websearch", "web_search"},
		{"search", "web_search"},
		// web_fetch aliases
		{"web_fetch", "web_fetch"},
		{"webfetch", "web_fetch"},
		{"fetch", "web_fetch"},
		{"get", "web_fetch"},
		// read aliases
		{"read_file", "read"},
		{"readfile", "read"},
		{"read", "read"},
		{"open", "read"},
		// write aliases — canonical is "write"
		{"write_file", "write"},
		{"writefile", "write"},
		{"write", "write"},
		{"save", "write"},
		// exec aliases
		{"execute", "exec"},
		{"run", "exec"},
		{"exec", "exec"},
		{"shell", "exec"},
		{"bash", "exec"},
		// browser aliases
		{"browser", "browser"},
		{"browse", "browser"},
	}

	p := parser.New("fuzzy", "")

	for _, tc := range cases {
		t.Run(tc.alias+"→"+tc.wantName, func(t *testing.T) {
			t.Parallel()
			// Exercise through the markers strategy so we can drive the alias
			// normalisation path without touching unexported methods.
			input := "[TOOL:" + tc.alias + "|key=val]"
			intents := p.Parse(input)
			// Switch parser to markers for this subtest.
			mp := parser.New("markers", "")
			intents = mp.Parse(input)
			if len(intents) == 0 {
				t.Fatalf("alias %q: expected an intent, got none", tc.alias)
			}
			if intents[0].Name != tc.wantName {
				t.Errorf("alias %q: Name = %q, want %q", tc.alias, intents[0].Name, tc.wantName)
			}
		})
	}
}

// TestFallback verifies that when the primary strategy finds nothing the
// fallback strategy is invoked and its results are returned.
func TestFallback(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		primary  string
		fallback string
		input    string
		wantName string
	}{
		{
			name:     "guided_json fails falls back to fuzzy",
			primary:  "guided_json",
			fallback: "fuzzy",
			input:    "search for Rust async programming",
			wantName: "web_search",
		},
		{
			name:     "react fails falls back to markers",
			primary:  "react",
			fallback: "markers",
			input:    "[TOOL:exec|command=uname -a]",
			wantName: "exec",
		},
		{
			name:     "markers fails falls back to react",
			primary:  "markers",
			fallback: "react",
			input: `Action: web_fetch
Action Input: {"url": "https://example.com"}`,
			wantName: "web_fetch",
		},
		{
			name:     "guided_json fails falls back to react",
			primary:  "guided_json",
			fallback: "react",
			input: `Action: web_search
Action Input: {"query": "fallback test"}`,
			wantName: "web_search",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := parser.New(tc.primary, tc.fallback)
			intents := p.Parse(tc.input)
			if len(intents) == 0 {
				t.Fatalf("expected fallback to produce intents, got none")
			}
			if intents[0].Name != tc.wantName {
				t.Errorf("Name = %q, want %q", intents[0].Name, tc.wantName)
			}
		})
	}
}

// TestUnknownStrategy verifies that an unrecognised strategy name produces no
// intents rather than panicking.
func TestUnknownStrategy(t *testing.T) {
	t.Parallel()
	p := parser.New("nonexistent", "")
	intents := p.Parse("search for something")
	if len(intents) != 0 {
		t.Errorf("unknown strategy: expected no intents, got %d", len(intents))
	}
}

// TestEmptyFallback verifies that an empty fallback string does not cause a
// panic or unexpected behaviour when the primary returns nothing.
func TestEmptyFallback(t *testing.T) {
	t.Parallel()
	p := parser.New("guided_json", "")
	intents := p.Parse("this is not json at all")
	if len(intents) != 0 {
		t.Errorf("expected no intents, got %d", len(intents))
	}
}
