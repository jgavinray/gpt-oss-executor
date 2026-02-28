// Package parser implements a 4-tier intent parser that extracts tool call
// intents from LLM output text. Tiers are tried in priority order:
// guided_json → react → markers → fuzzy. The primary strategy is attempted
// first; if it produces no results the fallback strategy is tried.
package parser

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
)

// ToolIntent represents a single tool invocation extracted from model output.
type ToolIntent struct {
	// Name is the canonical tool name, normalised through toolAliases.
	Name string
	// Args holds the tool arguments as string key-value pairs.
	Args map[string]string
	// Confidence is a value in [0.0, 1.0] indicating parser certainty.
	Confidence float32
}

// IntentParser extracts ToolIntents from LLM output using a configurable
// parse strategy with an optional fallback.
type IntentParser struct {
	// Strategy is the primary parse strategy name.
	// Valid values: "guided_json", "react", "markers", "fuzzy".
	Strategy string
	// FallbackStrategy is the secondary strategy used when the primary
	// returns no results. Same valid values as Strategy.
	FallbackStrategy string

	// fuzzyArgPatterns holds multiple compiled patterns per tool for argument
	// extraction — first match wins.
	fuzzyArgPatterns map[string][]*regexp.Regexp
	// fuzzyIntentPatterns holds broad keyword patterns that detect tool intent
	// even when a specific argument cannot be extracted from the reasoning text.
	fuzzyIntentPatterns map[string][]*regexp.Regexp
	toolAliases         map[string]string
}

// toolAliases maps every known surface spelling to a canonical tool name that
// matches the openclaw /tools/invoke `tool` field exactly.
var defaultAliases = map[string]string{
	"web_search": "web_search",
	"websearch":  "web_search",
	"search":     "web_search",

	"web_fetch": "web_fetch",
	"webfetch":  "web_fetch",
	"fetch":     "web_fetch",
	"get":       "web_fetch",

	"read_file": "read",
	"readfile":  "read",
	"read":      "read",
	"open":      "read",

	"write_file": "write",
	"writefile":  "write",
	"write":      "write",
	"save":       "write",

	"execute": "exec",
	"run":     "exec",
	"exec":    "exec",
	"shell":   "exec",
	"bash":    "exec",

	"browser": "browser",
	"browse":  "browser",
}

// fuzzyArgPatternDefs holds multiple raw pattern strings per tool for Tier 4
// argument extraction. Patterns are tried in order; the first capture group of
// the first matching pattern is used as the argument value.
var fuzzyArgPatternDefs = map[string][]string{
	"web_search": {
		// "search for X", "look up X", "find X"
		`(?i)(?:search|look\s+up|query|find)\s+(?:for\s+)?["']?(.+?)["']?(?:\s+(?:on|using|via|with)|[.\n]|$)`,
		// "browse the web to get/find X"
		`(?i)browse\s+(?:the\s+)?web\s+(?:to\s+(?:get|find|look\s+up|retrieve|check)\s+)?["']?(.+?)["']?(?:[.\n]|$)`,
		// "need/want/going to search/browse/google X"
		`(?i)(?:need|want|should|going)\s+to\s+(?:search|browse|look\s+up|google|find|check)\s+(?:(?:the\s+)?web\s+)?(?:for\s+|to\s+get\s+|about\s+)?["']?(.+?)["']?(?:[.\n]|$)`,
		// "to get the current X", "to find the X"
		`(?i)\bto\s+(?:get|find|retrieve|check|look\s+up)\s+(?:the\s+)?(?:current\s+)?["']?(.+?)["']?(?:[.\n]|$)`,
	},
	"web_fetch": {
		// "fetch/get the page at URL"
		`(?i)(?:fetch|retrieve|get|download|open)\s+(?:the\s+)?(?:page|url|site|content)?\s*(?:at|from)?\s*(https?://\S+)`,
	},
	"read": {
		"(?i)(?:read|open|view|check|load)\\s+(?:the\\s+)?(?:file|contents?\\s+of\\s+)?\\s*[\"'`]?([/~][\\w.\\-/]+)[\"'`]?",
	},
	"write": {
		"(?i)(?:write|save|create|output)\\s+(?:to|as|the file)\\s+[\"'`]?([/~][\\w.\\-/]+)[\"'`]?",
	},
	"exec": {
		"(?i)(?:run|execute|exec)\\s+(?:the\\s+)?(?:command|shell|bash)?\\s*[\"'`]([^\"'`\\n]+)[\"'`]",
	},
}

// fuzzyIntentPatternDefs holds broad keyword patterns that detect tool intent
// when specific argument extraction fails. If any pattern matches, an intent
// is returned with an empty argument so the executor can fall back to the
// original user message as the query/input.
var fuzzyIntentPatternDefs = map[string][]string{
	"web_search": {
		// Explicit search verbs
		`(?i)\b(?:search|browse\s+(?:the\s+)?web|look\s+up|google|web\s+search)\b`,
		`(?i)\buse\s+(?:search|web_search)\b`,
		`(?i)\b(?:search|browse)\s+(?:the\s+)?(?:web|internet|online)\b`,
		// Real-time / current-data signals — user is asking for live info
		`(?i)\b(?:current|today'?s?|latest|live|right\s+now|real[\s-]?time)\b.{0,40}\b(?:price|rate|stock|value|cost|quote|news|score|weather|temperature)\b`,
		`(?i)\b(?:price|rate|stock|value|cost|quote|news|score|weather|temperature)\b.{0,40}\b(?:current|today|latest|live|now|real[\s-]?time)\b`,
		// What is X today / right now
		`(?i)\bwhat\s+is\s+(?:the\s+)?(?:current|today'?s?|latest)\b`,
	},
	"web_fetch": {
		`(?i)\b(?:fetch|retrieve|download)\s+(?:the\s+)?(?:url|page|site)\b`,
	},
}

// fuzzyArgKeys maps each canonical tool name to the argument key that the
// first captured group should be stored under.
var fuzzyArgKeys = map[string]string{
	"web_search": "query",
	"web_fetch":  "url",
	"read":       "path",
	"write":      "path",
	"exec":       "command",
}

// New constructs an IntentParser with the given primary and fallback strategies.
// strategy and fallback must each be one of: "guided_json", "react",
// "markers", "fuzzy". An empty string for fallback disables the fallback tier.
func New(strategy, fallback string) *IntentParser {
	aliases := make(map[string]string, len(defaultAliases))
	for k, v := range defaultAliases {
		aliases[k] = v
	}

	argPatterns := make(map[string][]*regexp.Regexp, len(fuzzyArgPatternDefs))
	for tool, raws := range fuzzyArgPatternDefs {
		compiled := make([]*regexp.Regexp, 0, len(raws))
		for _, raw := range raws {
			compiled = append(compiled, regexp.MustCompile(raw))
		}
		argPatterns[tool] = compiled
	}

	intentPatterns := make(map[string][]*regexp.Regexp, len(fuzzyIntentPatternDefs))
	for tool, raws := range fuzzyIntentPatternDefs {
		compiled := make([]*regexp.Regexp, 0, len(raws))
		for _, raw := range raws {
			compiled = append(compiled, regexp.MustCompile(raw))
		}
		intentPatterns[tool] = compiled
	}

	return &IntentParser{
		Strategy:            strategy,
		FallbackStrategy:    fallback,
		fuzzyArgPatterns:    argPatterns,
		fuzzyIntentPatterns: intentPatterns,
		toolAliases:         aliases,
	}
}

// Parse extracts tool intents from text using the configured primary strategy.
// If the primary strategy returns no intents and a fallback strategy is set,
// the fallback is tried. Results are deduplicated by tool name.
func (p *IntentParser) Parse(text string) []ToolIntent {
	results := p.runStrategy(p.Strategy, text)
	if len(results) == 0 && p.FallbackStrategy != "" {
		slog.Debug("parser: primary strategy returned no results, trying fallback",
			"primary", p.Strategy,
			"fallback", p.FallbackStrategy,
		)
		results = p.runStrategy(p.FallbackStrategy, text)
	}
	return results
}

// runStrategy dispatches to the named strategy implementation.
func (p *IntentParser) runStrategy(strategy, text string) []ToolIntent {
	switch strategy {
	case "guided_json":
		return p.parseGuidedJSON(text)
	case "react":
		return p.parseReAct(text)
	case "markers":
		return p.parseMarkers(text)
	case "fuzzy":
		return p.parseFuzzy(text)
	default:
		slog.Warn("parser: unknown strategy, no intents extracted", "strategy", strategy)
		return nil
	}
}

// ---------------------------------------------------------------------------
// Tier 1: Guided JSON
// ---------------------------------------------------------------------------

// guidedJSONPayload is the expected shape of structured model output.
type guidedJSONPayload struct {
	Reasoning string           `json:"reasoning"`
	ToolCalls []guidedToolCall `json:"tool_calls"`
	Done      bool             `json:"done"`
}

type guidedToolCall struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

// parseGuidedJSON handles Tier 1: the model is expected to emit a JSON
// document with a `tool_calls` array and a `done` boolean.
//
// Attempts:
//  1. json.Unmarshal on the full text.
//  2. Extraction from a ```json ... ``` code fence.
//
// Returns early with an empty slice when done==true and tool_calls is empty,
// signalling task completion. Confidence is 1.0.
func (p *IntentParser) parseGuidedJSON(text string) []ToolIntent {
	payload, ok := p.unmarshalGuidedJSON(text)
	if !ok {
		// Try extracting from a code fence.
		fenced := extractJSONCodeBlock(text)
		if fenced == "" {
			return nil
		}
		payload, ok = p.unmarshalGuidedJSON(fenced)
		if !ok {
			return nil
		}
	}

	// done==true with no tool_calls signals task completion; return nothing.
	if payload.Done && len(payload.ToolCalls) == 0 {
		slog.Debug("parser: guided_json: done=true, no tool_calls — task complete")
		return nil
	}

	var intents []ToolIntent
	for _, tc := range payload.ToolCalls {
		canonical := p.normalizeTool(tc.Name)
		if canonical == "" {
			slog.Warn("parser: guided_json: unknown tool name, skipping",
				"name", tc.Name,
			)
			continue
		}
		if intentExists(intents, canonical) {
			continue
		}
		intents = append(intents, ToolIntent{
			Name:       canonical,
			Args:       argsToStrings(tc.Arguments),
			Confidence: 1.0,
		})
	}
	return intents
}

// unmarshalGuidedJSON attempts to decode raw into a guidedJSONPayload.
func (p *IntentParser) unmarshalGuidedJSON(raw string) (guidedJSONPayload, bool) {
	var payload guidedJSONPayload
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &payload); err != nil {
		return guidedJSONPayload{}, false
	}
	return payload, true
}

// extractJSONCodeBlock returns the content of the first ```json ... ``` fence
// found in text, or "" if none is present.
func extractJSONCodeBlock(text string) string {
	re := regexp.MustCompile("(?s)```json\\s*\\n?(.*?)\\n?```")
	m := re.FindStringSubmatch(text)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// ---------------------------------------------------------------------------
// Tier 2: ReAct
// ---------------------------------------------------------------------------

// actionRe matches lines of the form "Action: <name>" at the start of a line.
var actionRe = regexp.MustCompile(`(?m)^Action:\s*(\S+)\s*$`)

// actionInputRe matches lines of the form "Action Input: <value>" anywhere.
var actionInputRe = regexp.MustCompile(`(?m)^Action Input:\s*(.+)$`)

// parseReAct handles Tier 2: the ReAct prompting format where the model
// emits "Action:" / "Action Input:" line pairs. Confidence is 0.9.
func (p *IntentParser) parseReAct(text string) []ToolIntent {
	actionMatches := actionRe.FindAllStringSubmatchIndex(text, -1)
	if len(actionMatches) == 0 {
		return nil
	}

	var intents []ToolIntent

	for i, match := range actionMatches {
		nameStart, nameEnd := match[2], match[3]
		rawName := text[nameStart:nameEnd]

		// "done" action signals the model is finished; stop processing.
		if strings.EqualFold(rawName, "done") {
			slog.Debug("parser: react: Action: done — stopping", "index", i)
			break
		}

		canonical := p.normalizeTool(rawName)
		if canonical == "" {
			slog.Warn("parser: react: unknown tool name, skipping", "name", rawName)
			continue
		}
		if intentExists(intents, canonical) {
			continue
		}

		// Find the first "Action Input:" that appears after this Action match.
		actionEnd := match[1] // end of the full Action: line
		remaining := text[actionEnd:]
		inputMatch := actionInputRe.FindStringSubmatch(remaining)

		args := make(map[string]string)
		if inputMatch != nil {
			rawInput := strings.TrimSpace(inputMatch[1])
			if err := json.Unmarshal([]byte(rawInput), &args); err != nil {
				// Fallback: store the raw string under the "input" key.
				args["input"] = rawInput
			}
		}

		intents = append(intents, ToolIntent{
			Name:       canonical,
			Args:       args,
			Confidence: 0.9,
		})
	}

	return intents
}

// ---------------------------------------------------------------------------
// Tier 3: Markers
// ---------------------------------------------------------------------------

// markerRe matches [TOOL:name|key=val|key2=val2] with tolerance for spaces.
var markerRe = regexp.MustCompile(`(?i)\[\s*TOOL\s*:\s*(\w+)\s*\|([^\]]+)\]`)

// parseMarkers handles Tier 3: custom [TOOL:name|key=val] inline markers.
// Confidence is 0.85.
func (p *IntentParser) parseMarkers(text string) []ToolIntent {
	matches := markerRe.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}

	var intents []ToolIntent

	for _, m := range matches {
		rawName := strings.TrimSpace(m[1])
		rawPairs := m[2]

		canonical := p.normalizeTool(rawName)
		if canonical == "" {
			slog.Warn("parser: markers: unknown tool name, skipping", "name", rawName)
			continue
		}
		if intentExists(intents, canonical) {
			continue
		}

		args := make(map[string]string)
		for _, segment := range strings.Split(rawPairs, "|") {
			segment = strings.TrimSpace(segment)
			if segment == "" {
				continue
			}
			parts := strings.SplitN(segment, "=", 2)
			if len(parts) == 2 {
				key := strings.TrimSpace(parts[0])
				val := strings.TrimSpace(parts[1])
				if key != "" {
					args[key] = val
				}
			}
		}

		intents = append(intents, ToolIntent{
			Name:       canonical,
			Args:       args,
			Confidence: 0.85,
		})
	}

	return intents
}

// ---------------------------------------------------------------------------
// Tier 4: Fuzzy / natural language
// ---------------------------------------------------------------------------

// parseFuzzy handles Tier 4: heuristic natural-language pattern matching.
//
// For each tool it first tries to extract a specific argument using the
// multi-pattern list in fuzzyArgPatterns (first match wins). If no argument
// pattern matches but a broad intent keyword is detected via
// fuzzyIntentPatterns, an intent is still returned with an empty argument
// value — the executor is expected to substitute the original user query.
//
// Confidence is 0.6 with a specific argument, 0.4 for intent-only matches.
func (p *IntentParser) parseFuzzy(text string) []ToolIntent {
	var intents []ToolIntent

	// Iterate in a deterministic order so test output is stable.
	toolOrder := []string{"web_search", "web_fetch", "read", "write", "exec"}

	for _, tool := range toolOrder {
		if intentExists(intents, tool) {
			continue
		}

		argKey := fuzzyArgKeys[tool]

		// Phase 1: try to extract a specific argument value.
		var matchedVal string
		for _, re := range p.fuzzyArgPatterns[tool] {
			m := re.FindStringSubmatch(text)
			if m != nil && len(m) >= 2 {
				val := strings.TrimSpace(m[1])
				if val != "" {
					matchedVal = val
					break
				}
			}
		}

		if matchedVal != "" {
			intents = append(intents, ToolIntent{
				Name:       tool,
				Args:       map[string]string{argKey: matchedVal},
				Confidence: 0.6,
			})
			continue
		}

		// Phase 2: detect broad intent even without an extractable argument.
		// Return the intent with an empty arg value; the executor will
		// substitute the original user message as the query/input.
		intentDetected := false
		for _, re := range p.fuzzyIntentPatterns[tool] {
			if re.MatchString(text) {
				intentDetected = true
				break
			}
		}
		if intentDetected {
			intents = append(intents, ToolIntent{
				Name:       tool,
				Args:       map[string]string{argKey: ""},
				Confidence: 0.4,
			})
		}
	}

	return intents
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// normalizeTool returns the canonical tool name for alias, or "" if the alias
// is not recognised.
func (p *IntentParser) normalizeTool(alias string) string {
	return p.toolAliases[strings.ToLower(strings.TrimSpace(alias))]
}

// intentExists reports whether any intent in the slice already has the given
// canonical tool name. Deduplication is intentionally coarse (one of each tool
// per parse iteration is sufficient for the agentic loop).
func intentExists(intents []ToolIntent, name string) bool {
	for _, t := range intents {
		if t.Name == name {
			return true
		}
	}
	return false
}

// argsToStrings converts a map[string]interface{} (from JSON unmarshalling)
// to a map[string]string by formatting each value with fmt.Sprintf.
func argsToStrings(in map[string]interface{}) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = fmt.Sprintf("%v", v)
	}
	return out
}
