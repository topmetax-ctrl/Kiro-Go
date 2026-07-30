package proxy

import (
	"fmt"
	"strings"

	"kiro-go/config"
)

// ThinkingEffort is the reasoning-depth level requested for a single turn.
//
// It mirrors Anthropic's `output_config.effort` enum (low/medium/high/xhigh/max)
// plus EffortAuto, which means "think, but let the model pick the depth" — the
// state a request lands in when it asks for thinking without naming a level
// (e.g. `thinking: {type: "adaptive"}`, or the bare `-thinking` model suffix).
//
// IMPORTANT: Kiro's upstream (CodeWhisperer generateAssistantResponse) has no
// effort or thinking-budget field — its own client exposes thinking as a plain
// boolean, and InferenceConfig carries only maxTokens/temperature/topP. So the
// level cannot be forwarded as a request parameter. The only lever available is
// the <max_thinking_length> hint in the injected system prompt, which is a
// behavioral steer, not an enforced budget. Callers should not expect the
// upstream to hard-cap thinking at the mapped value.
type ThinkingEffort string

const (
	// EffortUnset means no thinking was requested (thinking stays off).
	EffortUnset ThinkingEffort = ""
	// EffortAuto means thinking is on with no explicit depth.
	EffortAuto   ThinkingEffort = "auto"
	EffortLow    ThinkingEffort = "low"
	EffortMedium ThinkingEffort = "medium"
	EffortHigh   ThinkingEffort = "high"
	EffortXHigh  ThinkingEffort = "xhigh"
	EffortMax    ThinkingEffort = "max"
)

// defaultThinkingBudget is the <max_thinking_length> value used for EffortAuto
// and EffortHigh. It matches the historical hard-coded value, so requests that
// do not name a level behave exactly as they did before levels existed.
// Anthropic also documents `high` as the API default, so auto and high sharing
// this value is consistent with upstream semantics.
const defaultThinkingBudget = 200000

// thinkingEffortBudgets maps a level to the <max_thinking_length> hint injected
// into the system prompt. These are prompt-level steers chosen to spread across
// roughly an order of magnitude; they are not upstream token budgets and Kiro
// does not enforce them.
var thinkingEffortBudgets = map[ThinkingEffort]int{
	EffortLow:    8000,
	EffortMedium: 32000,
	EffortHigh:   defaultThinkingBudget,
	EffortXHigh:  400000,
	EffortMax:    600000,
}

// thinkingEffortAliases normalizes the spellings seen in the wild to the
// canonical enum. Note `adaptive`: Anthropic's docs say explicitly not to pass
// it as an effort value ("adaptive is a thinking mode, not an effort level"),
// but clients do send it in the effort slot. Mapping it to EffortAuto is more
// useful here than rejecting the request, since auto is what it means.
var thinkingEffortAliases = map[string]ThinkingEffort{
	"auto":      EffortAuto,
	"adaptive":  EffortAuto,
	"default":   EffortAuto,
	"low":       EffortLow,
	"minimal":   EffortLow,
	"medium":    EffortMedium,
	"med":       EffortMedium,
	"high":      EffortHigh,
	"xhigh":     EffortXHigh,
	"x-high":    EffortXHigh,
	"x_high":    EffortXHigh,
	"extrahigh": EffortXHigh,
	"max":       EffortMax,
	"maximum":   EffortMax,
}

// NormalizeThinkingEffort resolves a raw client-supplied level to the canonical
// enum. It reports false when the value is non-empty but unrecognized, so
// callers can reject it the way the Anthropic API rejects an invalid effort.
func NormalizeThinkingEffort(raw string) (ThinkingEffort, bool) {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	if trimmed == "" {
		return EffortUnset, true
	}
	// Tolerate the parenthesized form ("(xhigh)") arriving as a bare value.
	trimmed = strings.Trim(trimmed, "()")
	trimmed = strings.TrimSpace(trimmed)
	if trimmed == "" {
		return EffortUnset, true
	}
	if level, ok := thinkingEffortAliases[trimmed]; ok {
		return level, true
	}
	return EffortUnset, false
}

// orderedThinkingEfforts lists the explicit levels shallowest-first. EffortAuto
// is excluded: it means "no explicit level", so it is not something a caller
// picks from a list. Iteration order matters for /v1/models and error messages,
// which is why this is a slice and not a range over thinkingEffortBudgets.
var orderedThinkingEfforts = []ThinkingEffort{
	EffortLow,
	EffortMedium,
	EffortHigh,
	EffortXHigh,
	EffortMax,
}

// ThinkingEffortValues lists the accepted levels for error messages.
func ThinkingEffortValues() string {
	parts := make([]string, 0, len(orderedThinkingEfforts))
	for _, level := range orderedThinkingEfforts {
		parts = append(parts, string(level))
	}
	return strings.Join(parts, ", ")
}

// thinkingBudgetForEffort returns the <max_thinking_length> hint for a level.
func thinkingBudgetForEffort(effort ThinkingEffort) int {
	if budget, ok := thinkingEffortBudgets[effort]; ok {
		return budget
	}
	return defaultThinkingBudget
}

// effortFromBudgetTokens maps a legacy `thinking.budget_tokens` value onto the
// level ladder. Anthropic deprecated budget_tokens on the 4.6 models and 4.7+
// reject it outright, but clients still send it, so it is honored here as a
// depth hint rather than dropped. Thresholds bracket each level's mapped budget.
func effortFromBudgetTokens(budget int) ThinkingEffort {
	switch {
	case budget <= 0:
		return EffortUnset
	case budget <= 12000:
		return EffortLow
	case budget <= 48000:
		return EffortMedium
	case budget <= 262144:
		return EffortHigh
	case budget <= 524288:
		return EffortXHigh
	default:
		return EffortMax
	}
}

// thinkingModePromptForEffort builds the system-prompt priming block for a
// level. The tag shape is unchanged from the original fixed prompt; only the
// budget varies, so EffortAuto/EffortHigh reproduce ThinkingModePrompt byte for
// byte.
func thinkingModePromptForEffort(effort ThinkingEffort) string {
	budget := thinkingBudgetForEffort(effort)
	if budget == defaultThinkingBudget {
		return ThinkingModePrompt
	}
	return fmt.Sprintf("<thinking_mode>enabled</thinking_mode>\n<max_thinking_length>%d</max_thinking_length>", budget)
}

// resolveEffortForPayload combines a caller's on/off thinking decision with the
// depth hint carried on the request. The boolean gates whether thinking happens
// at all; the request supplies how deep. This keeps the existing
// ClaudeToKiro/OpenAIToKiro contracts intact while letting levels flow through.
//
// When the request names no level, the admin-configured default fills in. That
// default only applies to requests that stayed silent, so a client asking for a
// specific level always wins.
func resolveEffortForPayload(thinking bool, requested ThinkingEffort) ThinkingEffort {
	return resolveEffortWithDefault(thinking, requested, configuredDefaultEffort())
}

// configuredDefaultEffort reads the admin default and normalizes it. A stored
// value that no longer parses is treated as unset rather than failing the
// request, since the level is only ever a prompt hint.
func configuredDefaultEffort() ThinkingEffort {
	level, ok := NormalizeThinkingEffort(config.GetThinkingConfig().DefaultEffort)
	if !ok {
		return EffortUnset
	}
	return level
}

// resolveEffortWithDefault is resolveEffortForPayload's logic with the config
// read lifted out, so the precedence is testable without touching global state.
func resolveEffortWithDefault(thinking bool, requested, fallback ThinkingEffort) ThinkingEffort {
	if !thinking {
		return EffortUnset
	}
	if requested != EffortUnset {
		return requested
	}
	if fallback != EffortUnset {
		return fallback
	}
	return EffortAuto
}

// stripParenLevel removes a trailing parenthesized level from a model name.
//
// 9router's provider page appends a "(level)" suffix to the model id you copy,
// and strips it from body.model before calling upstream. If it ever reaches us
// un-stripped, the bare name would fail Kiro's model validation, so this both
// recovers the level and cleans the name. Matching is strict: the parenthesized
// text must be a recognized level, otherwise the name is returned untouched.
func stripParenLevel(model string) (string, ThinkingEffort) {
	trimmed := strings.TrimRight(model, " \t")
	if !strings.HasSuffix(trimmed, ")") {
		return model, EffortUnset
	}
	open := strings.LastIndex(trimmed, "(")
	if open < 0 {
		return model, EffortUnset
	}
	level, ok := NormalizeThinkingEffort(trimmed[open+1 : len(trimmed)-1])
	if !ok || level == EffortUnset {
		return model, EffortUnset
	}
	return strings.TrimRight(trimmed[:open], " \t"), level
}

// claudeRequestEffort resolves the requested depth from an Anthropic-shaped
// request body, in precedence order:
//
//  1. output_config.effort — the current, documented field.
//  2. thinking.budget_tokens — the deprecated manual-mode budget.
//  3. thinking.type adaptive/enabled with no depth — EffortAuto.
//
// output_config.effort wins over budget_tokens because that is the direction
// Anthropic's own migration guide points, and because a client sending both
// (as 9router does for Claude-format providers) means the newer field.
func claudeRequestEffort(req *ClaudeRequest) ThinkingEffort {
	if req == nil {
		return EffortUnset
	}
	if req.OutputConfig != nil {
		if level, ok := NormalizeThinkingEffort(req.OutputConfig.Effort); ok && level != EffortUnset {
			return level
		}
	}
	if req.Thinking != nil {
		if level := effortFromBudgetTokens(req.Thinking.BudgetTokens); level != EffortUnset {
			return level
		}
	}
	if isClaudeThinkingRequested(req.Thinking) {
		return EffortAuto
	}
	return EffortUnset
}

// openAIRequestEffort resolves depth from an OpenAI-shaped request body, which
// spells the field `reasoning_effort`.
func openAIRequestEffort(req *OpenAIRequest) ThinkingEffort {
	if req == nil {
		return EffortUnset
	}
	if level, ok := NormalizeThinkingEffort(req.ReasoningEffort); ok {
		return level
	}
	return EffortUnset
}

// applyOpenAIModelNameEffort records a model-name level on an OpenAI-shaped
// request body, so the conversion path reads the level from one place. A level
// already present in the body wins, matching openAIRequestEffort's precedence.
func applyOpenAIModelNameEffort(req *OpenAIRequest, level ThinkingEffort) {
	if req == nil || level == EffortUnset {
		return
	}
	if strings.TrimSpace(req.ReasoningEffort) != "" {
		return
	}
	req.ReasoningEffort = string(level)
}

// validateThinkingEffortValue rejects an unrecognized effort string, mirroring
// the Anthropic API's 400 on an invalid enum value.
//
// Unlike upstream, no per-model gating is applied: Anthropic restricts xhigh and
// max to specific model families, but Kiro never sees the effort value at all,
// so gating here would reject requests that would otherwise succeed.
func validateThinkingEffortValue(raw string) string {
	if _, ok := NormalizeThinkingEffort(raw); !ok {
		return "output_config.effort must be one of: " + ThinkingEffortValues()
	}
	return ""
}

// validateClaudeOutputConfig validates the output_config block.
func validateClaudeOutputConfig(cfg *ClaudeOutputConfig) string {
	if cfg == nil {
		return ""
	}
	return validateThinkingEffortValue(cfg.Effort)
}
