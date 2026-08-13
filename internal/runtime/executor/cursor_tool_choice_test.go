package executor

import (
	"strings"
	"testing"
)

func buildFromPayload(t *testing.T, payload string) (*parsedOpenAIRequest, *struct {
	tools  int
	prompt string
}) {
	t.Helper()
	parsed := parseOpenAIRequest([]byte(payload))
	params := buildRunRequestParams(parsed, "conv", "model")
	return parsed, &struct {
		tools  int
		prompt string
	}{tools: len(params.McpTools), prompt: params.SystemPrompt}
}

const twoToolsPayload = `"tools":[` +
	`{"type":"function","function":{"name":"get_weather","description":"w","parameters":{"type":"object"}}},` +
	`{"type":"function","function":{"name":"get_time","description":"t","parameters":{"type":"object"}}}]`

func TestToolChoiceNoneDropsTools(t *testing.T) {
	parsed, got := buildFromPayload(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],`+twoToolsPayload+`,"tool_choice":"none"}`)
	if parsed.ToolChoice != "none" {
		t.Fatalf("ToolChoice = %q, want none", parsed.ToolChoice)
	}
	if got.tools != 0 {
		t.Fatalf("advertised %d tools with tool_choice=none, want 0", got.tools)
	}
}

func TestToolChoiceSpecificFiltersToNamedTool(t *testing.T) {
	parsed, got := buildFromPayload(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],`+twoToolsPayload+`,"tool_choice":{"type":"function","function":{"name":"get_time"}}}`)
	if parsed.ToolChoice != "tool:get_time" {
		t.Fatalf("ToolChoice = %q, want tool:get_time", parsed.ToolChoice)
	}
	if got.tools != 1 {
		t.Fatalf("advertised %d tools with a specific tool_choice, want 1", got.tools)
	}
	if !strings.Contains(got.prompt, `"get_time"`) {
		t.Fatalf("system prompt missing forced-tool directive: %q", got.prompt)
	}
}

func TestToolChoiceRequiredAddsDirective(t *testing.T) {
	_, got := buildFromPayload(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],`+twoToolsPayload+`,"tool_choice":"required"}`)
	if got.tools != 2 {
		t.Fatalf("advertised %d tools with tool_choice=required, want 2", got.tools)
	}
	if !strings.Contains(got.prompt, "must call at least one") {
		t.Fatalf("system prompt missing required-tool directive: %q", got.prompt)
	}
}

func TestToolChoiceAutoIsUnchanged(t *testing.T) {
	_, got := buildFromPayload(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],`+twoToolsPayload+`,"tool_choice":"auto"}`)
	if got.tools != 2 {
		t.Fatalf("advertised %d tools with tool_choice=auto, want 2", got.tools)
	}
	if strings.Contains(got.prompt, "must call") {
		t.Fatalf("auto tool_choice should not inject directives: %q", got.prompt)
	}
}

func TestParallelToolCallsDisabledAddsDirective(t *testing.T) {
	parsed, got := buildFromPayload(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],`+twoToolsPayload+`,"parallel_tool_calls":false}`)
	if parsed.ParallelToolCalls == nil || *parsed.ParallelToolCalls {
		t.Fatalf("ParallelToolCalls not parsed as false: %#v", parsed.ParallelToolCalls)
	}
	if !strings.Contains(got.prompt, "at most one tool per turn") {
		t.Fatalf("system prompt missing no-parallel directive: %q", got.prompt)
	}
}

func TestParallelToolCallsDefaultOmitsDirective(t *testing.T) {
	parsed, got := buildFromPayload(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],`+twoToolsPayload+`}`)
	if parsed.ParallelToolCalls != nil {
		t.Fatalf("ParallelToolCalls should be nil when absent, got %#v", parsed.ParallelToolCalls)
	}
	if strings.Contains(got.prompt, "at most one tool") {
		t.Fatalf("default should allow parallel calls: %q", got.prompt)
	}
}
