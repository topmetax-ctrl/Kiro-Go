package proxy

import (
	"strings"
	"testing"
)

func TestNormalizeThinkingEffort(t *testing.T) {
	tests := []struct {
		raw    string
		want   ThinkingEffort
		wantOK bool
	}{
		{"", EffortUnset, true},
		{"  ", EffortUnset, true},
		{"low", EffortLow, true},
		{"LOW", EffortLow, true},
		{" Medium ", EffortMedium, true},
		{"high", EffortHigh, true},
		{"xhigh", EffortXHigh, true},
		{"x-high", EffortXHigh, true},
		{"max", EffortMax, true},
		// adaptive is a thinking mode, not an effort level, but clients send it
		// in the effort slot; treat it as "think, depth unspecified".
		{"adaptive", EffortAuto, true},
		{"auto", EffortAuto, true},
		// Parenthesized form arriving as a bare value.
		{"(xhigh)", EffortXHigh, true},
		// Unrecognized values must be reportable so callers can 400.
		{"ultra", EffortUnset, false},
		{"none", EffortUnset, false},
	}

	for _, tc := range tests {
		got, ok := NormalizeThinkingEffort(tc.raw)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("NormalizeThinkingEffort(%q) = (%q, %v), want (%q, %v)",
				tc.raw, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestStripParenLevel(t *testing.T) {
	tests := []struct {
		model     string
		wantModel string
		wantLevel ThinkingEffort
	}{
		{"claude-opus-4.8 (xhigh)", "claude-opus-4.8", EffortXHigh},
		{"claude-opus-4.8(max)", "claude-opus-4.8", EffortMax},
		{"claude-sonnet-4.5 (low)", "claude-sonnet-4.5", EffortLow},
		// No suffix: untouched.
		{"claude-opus-4.8", "claude-opus-4.8", EffortUnset},
		// Unrecognized parenthetical: leave the name alone rather than
		// silently mangling a model id that merely contains parentheses.
		{"claude-opus-4.8 (turbo)", "claude-opus-4.8 (turbo)", EffortUnset},
		{"claude-opus-4.8 ()", "claude-opus-4.8 ()", EffortUnset},
	}

	for _, tc := range tests {
		gotModel, gotLevel := stripParenLevel(tc.model)
		if gotModel != tc.wantModel || gotLevel != tc.wantLevel {
			t.Errorf("stripParenLevel(%q) = (%q, %q), want (%q, %q)",
				tc.model, gotModel, gotLevel, tc.wantModel, tc.wantLevel)
		}
	}
}

func TestParseModelThinkingAndEffort(t *testing.T) {
	tests := []struct {
		name         string
		model        string
		wantModel    string
		wantThinking bool
		wantLevel    ThinkingEffort
	}{
		{
			name:         "paren level implies thinking and is stripped",
			model:        "claude-opus-4.8 (xhigh)",
			wantModel:    "claude-opus-4.8",
			wantThinking: true,
			wantLevel:    EffortXHigh,
		},
		{
			name:         "paren level combines with thinking suffix",
			model:        "claude-opus-4.8-thinking (low)",
			wantModel:    "claude-opus-4.8",
			wantThinking: true,
			wantLevel:    EffortLow,
		},
		{
			name:         "suffix alone leaves level unset",
			model:        "claude-sonnet-4.5-thinking",
			wantModel:    "claude-sonnet-4.5",
			wantThinking: true,
			wantLevel:    EffortUnset,
		},
		{
			name:         "plain model unchanged",
			model:        "claude-opus-4.8",
			wantModel:    "claude-opus-4.8",
			wantThinking: false,
			wantLevel:    EffortUnset,
		},
		{
			name:         "dash version still normalizes under a paren level",
			model:        "claude-opus-4-8 (max)",
			wantModel:    "claude-opus-4.8",
			wantThinking: true,
			wantLevel:    EffortMax,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotModel, gotThinking, gotLevel := ParseModelThinkingAndEffort(tc.model, "-thinking")
			if gotModel != tc.wantModel {
				t.Errorf("model = %q, want %q", gotModel, tc.wantModel)
			}
			if gotThinking != tc.wantThinking {
				t.Errorf("thinking = %v, want %v", gotThinking, tc.wantThinking)
			}
			if gotLevel != tc.wantLevel {
				t.Errorf("level = %q, want %q", gotLevel, tc.wantLevel)
			}
		})
	}
}

// An empty configured suffix must not mark every model as a thinking request,
// which is what a bare strings.HasSuffix(x, "") would do.
func TestParseModelAndThinkingEmptySuffix(t *testing.T) {
	model, thinking := ParseModelAndThinking("claude-opus-4.8", "")
	if thinking {
		t.Errorf("expected thinking=false with an empty suffix, got true")
	}
	if model != "claude-opus-4.8" {
		t.Errorf("model = %q, want %q", model, "claude-opus-4.8")
	}
}

func TestEffortFromBudgetTokens(t *testing.T) {
	tests := []struct {
		budget int
		want   ThinkingEffort
	}{
		{0, EffortUnset},
		{-1, EffortUnset},
		{1024, EffortLow},
		{10000, EffortLow},
		{32000, EffortMedium},
		{48000, EffortMedium},
		{100000, EffortHigh},
		{262144, EffortHigh},
		{400000, EffortXHigh},
		{1000000, EffortMax},
	}

	for _, tc := range tests {
		if got := effortFromBudgetTokens(tc.budget); got != tc.want {
			t.Errorf("effortFromBudgetTokens(%d) = %q, want %q", tc.budget, got, tc.want)
		}
	}
}

// Auto and high must reproduce the historical prompt byte for byte so requests
// that never name a level behave exactly as they did before levels existed.
func TestThinkingModePromptBackwardCompatible(t *testing.T) {
	for _, effort := range []ThinkingEffort{EffortAuto, EffortHigh} {
		if got := thinkingModePromptForEffort(effort); got != ThinkingModePrompt {
			t.Errorf("thinkingModePromptForEffort(%q) = %q, want the original prompt %q",
				effort, got, ThinkingModePrompt)
		}
	}
}

func TestThinkingModePromptVariesWithEffort(t *testing.T) {
	tests := []struct {
		effort     ThinkingEffort
		wantBudget string
	}{
		{EffortLow, "8000"},
		{EffortMedium, "32000"},
		{EffortXHigh, "400000"},
		{EffortMax, "600000"},
	}

	for _, tc := range tests {
		got := thinkingModePromptForEffort(tc.effort)
		if !strings.Contains(got, "<max_thinking_length>"+tc.wantBudget+"</max_thinking_length>") {
			t.Errorf("thinkingModePromptForEffort(%q) = %q, want budget %s", tc.effort, got, tc.wantBudget)
		}
		if !strings.Contains(got, "<thinking_mode>enabled</thinking_mode>") {
			t.Errorf("thinkingModePromptForEffort(%q) lost the enabled tag: %q", tc.effort, got)
		}
	}
}

func TestClaudeRequestEffortPrecedence(t *testing.T) {
	tests := []struct {
		name string
		req  *ClaudeRequest
		want ThinkingEffort
	}{
		{
			name: "nil request",
			req:  nil,
			want: EffortUnset,
		},
		{
			name: "output_config wins over budget_tokens",
			req: &ClaudeRequest{
				OutputConfig: &ClaudeOutputConfig{Effort: "low"},
				Thinking:     &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 400000},
			},
			want: EffortLow,
		},
		{
			name: "budget_tokens used when output_config absent",
			req: &ClaudeRequest{
				Thinking: &ClaudeThinkingConfig{Type: "enabled", BudgetTokens: 2048},
			},
			want: EffortLow,
		},
		{
			name: "adaptive with no depth is auto",
			req: &ClaudeRequest{
				Thinking: &ClaudeThinkingConfig{Type: "adaptive"},
			},
			want: EffortAuto,
		},
		{
			// 9router's Claude-format shape: adaptive plus output_config.effort.
			name: "9router shape resolves to the named level",
			req: &ClaudeRequest{
				Thinking:     &ClaudeThinkingConfig{Type: "adaptive"},
				OutputConfig: &ClaudeOutputConfig{Effort: "xhigh"},
			},
			want: EffortXHigh,
		},
		{
			name: "disabled thinking with no effort stays unset",
			req: &ClaudeRequest{
				Thinking: &ClaudeThinkingConfig{Type: "disabled"},
			},
			want: EffortUnset,
		},
		{
			// effort is independent of thinking per Anthropic's docs, so a level
			// with no thinking block still resolves.
			name: "effort alone resolves without a thinking block",
			req: &ClaudeRequest{
				OutputConfig: &ClaudeOutputConfig{Effort: "max"},
			},
			want: EffortMax,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeRequestEffort(tc.req); got != tc.want {
				t.Errorf("claudeRequestEffort = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveEffortForPayload(t *testing.T) {
	// Thinking off wins regardless of a requested level.
	if got := resolveEffortForPayload(false, EffortMax); got != EffortUnset {
		t.Errorf("thinking=false should suppress effort, got %q", got)
	}
	// Thinking on with no level named falls back to auto.
	if got := resolveEffortForPayload(true, EffortUnset); got != EffortAuto {
		t.Errorf("thinking=true with no level should be auto, got %q", got)
	}
	// Thinking on with a level passes it through.
	if got := resolveEffortForPayload(true, EffortLow); got != EffortLow {
		t.Errorf("expected low to pass through, got %q", got)
	}
}

func TestResolveEffortWithDefault(t *testing.T) {
	cases := []struct {
		name      string
		thinking  bool
		requested ThinkingEffort
		fallback  ThinkingEffort
		want      ThinkingEffort
	}{
		{"thinking off ignores both", false, EffortMax, EffortLow, EffortUnset},
		{"request wins over default", true, EffortLow, EffortMax, EffortLow},
		{"default fills a silent request", true, EffortUnset, EffortMedium, EffortMedium},
		{"no default falls back to auto", true, EffortUnset, EffortUnset, EffortAuto},
		{"auto default behaves as unset", true, EffortUnset, EffortAuto, EffortAuto},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveEffortWithDefault(tc.thinking, tc.requested, tc.fallback); got != tc.want {
				t.Errorf("resolveEffortWithDefault(%v, %q, %q) = %q, want %q",
					tc.thinking, tc.requested, tc.fallback, got, tc.want)
			}
		})
	}
}

func TestValidateClaudeOutputConfig(t *testing.T) {
	if msg := validateClaudeOutputConfig(nil); msg != "" {
		t.Errorf("nil output_config should be valid, got %q", msg)
	}
	if msg := validateClaudeOutputConfig(&ClaudeOutputConfig{}); msg != "" {
		t.Errorf("empty effort should be valid, got %q", msg)
	}
	for _, level := range []string{"low", "medium", "high", "xhigh", "max"} {
		if msg := validateClaudeOutputConfig(&ClaudeOutputConfig{Effort: level}); msg != "" {
			t.Errorf("effort %q should be valid, got %q", level, msg)
		}
	}
	if msg := validateClaudeOutputConfig(&ClaudeOutputConfig{Effort: "ultra"}); msg == "" {
		t.Errorf("expected an unrecognized effort to be rejected")
	}
}

func TestApplyModelNameEffortRespectsBody(t *testing.T) {
	// A level in the body wins over one carried by the model name.
	req := &ClaudeRequest{OutputConfig: &ClaudeOutputConfig{Effort: "low"}}
	applyModelNameEffort(req, EffortMax)
	if req.OutputConfig.Effort != "low" {
		t.Errorf("body effort should win, got %q", req.OutputConfig.Effort)
	}

	// With no body level, the model-name level is recorded.
	req = &ClaudeRequest{}
	applyModelNameEffort(req, EffortXHigh)
	if req.OutputConfig == nil || req.OutputConfig.Effort != "xhigh" {
		t.Errorf("expected model-name effort to be recorded, got %#v", req.OutputConfig)
	}

	// Unset level is a no-op.
	req = &ClaudeRequest{}
	applyModelNameEffort(req, EffortUnset)
	if req.OutputConfig != nil {
		t.Errorf("expected no output_config for an unset level, got %#v", req.OutputConfig)
	}
}

func TestApplyOpenAIModelNameEffortRespectsBody(t *testing.T) {
	req := &OpenAIRequest{ReasoningEffort: "low"}
	applyOpenAIModelNameEffort(req, EffortMax)
	if req.ReasoningEffort != "low" {
		t.Errorf("body reasoning_effort should win, got %q", req.ReasoningEffort)
	}

	req = &OpenAIRequest{}
	applyOpenAIModelNameEffort(req, EffortMedium)
	if req.ReasoningEffort != "medium" {
		t.Errorf("expected model-name effort to be recorded, got %q", req.ReasoningEffort)
	}
}

// systemPrimingFromPayload returns the serialized payload so tests can assert on
// the injected priming block without depending on its exact position in history.
// systemPrimingFromPayload returns the system-priming text that ClaudeToKiro /
// OpenAIToKiro placed at the head of history. Read off the struct rather than
// marshaled JSON, since json.Marshal HTML-escapes the "<" in the thinking tags.
func systemPrimingFromPayload(t *testing.T, payload *KiroPayload) string {
	t.Helper()
	if payload == nil {
		t.Fatal("nil payload")
	}
	history := payload.ConversationState.History
	if len(history) == 0 || history[0].UserInputMessage == nil {
		return ""
	}
	return history[0].UserInputMessage.Content
}

func TestClaudeToKiroInjectsEffortBudget(t *testing.T) {
	tests := []struct {
		name       string
		req        *ClaudeRequest
		thinking   bool
		wantBudget string
		wantNoTag  bool
	}{
		{
			name: "low effort narrows the budget",
			req: &ClaudeRequest{
				Model:        "claude-opus-4.8",
				Messages:     []ClaudeMessage{{Role: "user", Content: "hi"}},
				Thinking:     &ClaudeThinkingConfig{Type: "adaptive"},
				OutputConfig: &ClaudeOutputConfig{Effort: "low"},
			},
			thinking:   true,
			wantBudget: "8000",
		},
		{
			name: "xhigh effort widens the budget",
			req: &ClaudeRequest{
				Model:        "claude-opus-4.8",
				Messages:     []ClaudeMessage{{Role: "user", Content: "hi"}},
				Thinking:     &ClaudeThinkingConfig{Type: "adaptive"},
				OutputConfig: &ClaudeOutputConfig{Effort: "xhigh"},
			},
			thinking:   true,
			wantBudget: "400000",
		},
		{
			name: "no level keeps the historical budget",
			req: &ClaudeRequest{
				Model:    "claude-opus-4.8",
				Messages: []ClaudeMessage{{Role: "user", Content: "hi"}},
				Thinking: &ClaudeThinkingConfig{Type: "adaptive"},
			},
			thinking:   true,
			wantBudget: "200000",
		},
		{
			name: "thinking off injects nothing",
			req: &ClaudeRequest{
				Model:    "claude-opus-4.8",
				Messages: []ClaudeMessage{{Role: "user", Content: "hi"}},
			},
			thinking:  false,
			wantNoTag: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := systemPrimingFromPayload(t, ClaudeToKiro(tc.req, tc.thinking))
			if tc.wantNoTag {
				if strings.Contains(body, "thinking_mode") {
					t.Fatalf("expected no thinking priming, got: %s", body)
				}
				return
			}
			want := "<max_thinking_length>" + tc.wantBudget + "</max_thinking_length>"
			if !strings.Contains(body, want) {
				t.Fatalf("expected priming to contain %q, got: %s", want, body)
			}
		})
	}
}

func TestOpenAIToKiroInjectsEffortBudget(t *testing.T) {
	req := &OpenAIRequest{
		Model:           "claude-opus-4.8",
		Messages:        []OpenAIMessage{{Role: "user", Content: "hi"}},
		ReasoningEffort: "low",
	}
	body := systemPrimingFromPayload(t, OpenAIToKiro(req, true))
	if !strings.Contains(body, "<max_thinking_length>8000</max_thinking_length>") {
		t.Fatalf("expected low-effort budget in priming, got: %s", body)
	}

	// reasoning_effort must not turn thinking on by itself; the caller's boolean
	// gates that, matching the existing OpenAIToKiro contract.
	off := systemPrimingFromPayload(t, OpenAIToKiro(req, false))
	if strings.Contains(off, "thinking_mode") {
		t.Fatalf("expected no priming when thinking is off, got: %s", off)
	}
}

// The token estimate must track the priming block that is actually sent,
// otherwise budgeting and cache accounting drift from the real request.
func TestEffortAffectsClaudeTokenEstimate(t *testing.T) {
	base := &ClaudeRequest{
		Model:    "claude-opus-4.8",
		Messages: []ClaudeMessage{{Role: "user", Content: "hello"}},
		Thinking: &ClaudeThinkingConfig{Type: "adaptive"},
	}

	withLevel := func(level string) int {
		req := *base
		req.OutputConfig = &ClaudeOutputConfig{Effort: level}
		return estimateClaudeRequestInputTokens(cloneClaudeRequestForThinking(&req, true))
	}

	// The budget digits differ in length, so the estimates must differ.
	if withLevel("low") == withLevel("max") {
		t.Errorf("expected low and max priming to produce different estimates")
	}

	noThinking := estimateClaudeRequestInputTokens(cloneClaudeRequestForThinking(base, false))
	if withLevel("low") <= noThinking {
		t.Errorf("expected thinking priming to raise the estimate above %d", noThinking)
	}
}
