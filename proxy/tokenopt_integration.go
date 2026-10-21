package proxy

import (
	"context"
	"encoding/json"
	"strings"

	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/tokenopt"
)

// Glue between the request handlers and package tokenopt. The handlers call
// three things, in this order, and nothing else:
//
//	run := h.tokenOptBegin()                    // one settings snapshot per request
//	res := run.input(...)                       // input compression (may shrink tool results)
//	fwd, behavior := run.applyBehavior(...)     // Caveman / Ponytail prompt text
//
// With the feature off, tokenOptBegin returns a run with no manager and every
// method returns its input unchanged, so a disabled install pays one config read.

// tokenOptManager returns the shared Manager, creating it on first use.
func (h *Handler) tokenOptManager() *tokenopt.Manager {
	h.tokenOptOnce.Do(func() { h.tokenOptMgr = tokenopt.NewManager(estimateApproxTokens) })
	return h.tokenOptMgr
}

// closeTokenOpt stops the manager's background work. Nothing is built after it.
func (h *Handler) closeTokenOpt() {
	h.tokenOptOnce.Do(func() {})
	if h.tokenOptMgr != nil {
		h.tokenOptMgr.Close()
	}
}

// tokenOptRun is one request's view of Token Optimization: the manager and the
// settings snapshot, fixed for the request's lifetime so a settings change
// mid-request cannot make its input and prompt steps disagree.
type tokenOptRun struct {
	mgr *tokenopt.Manager
	cfg config.TokenOptimizationConfig
}

func (h *Handler) tokenOptBegin() tokenOptRun {
	cfg := config.GetTokenOptimization()
	if cfg.Mode == config.TokenOptModeOff {
		return tokenOptRun{cfg: cfg}
	}
	return tokenOptRun{mgr: h.tokenOptManager(), cfg: cfg}
}

// input offers a request body for input compression. The result's Body is the
// client's own body unless Changed.
func (r tokenOptRun) input(ctx context.Context, proto tokenopt.Protocol, body []byte, model string) tokenopt.InputResult {
	if r.mgr == nil {
		return tokenopt.InputResult{Body: body}
	}
	return r.mgr.OptimizeInput(ctx, r.cfg, tokenopt.InputRequest{
		Protocol: proto, Body: body, Model: model, Principal: memoryScopeForRequest(ctx),
	})
}

// applyBehavior decides what Caveman/Ponytail text this request carries and
// returns it in the two forms the two paths need:
//
//   - fwdBody: the raw body with the block appended to its system prompt. Only
//     built when the model resolves to an external provider route, since only
//     that path relays the body as written.
//   - kiro: the block for the Kiro-native translator, which appends it after its
//     own prompt filters and dedups against its final prompt.
//
// Both are no-ops unless the engine mode is "on" and a style or level is chosen.
func (r tokenOptRun) applyBehavior(proto tokenopt.Protocol, model string, body []byte) (fwdBody []byte, kiro tokenopt.Behavior) {
	fwdBody = body
	if r.mgr == nil {
		return fwdBody, kiro
	}
	kiro = r.mgr.WantedBehavior(r.cfg)
	if kiro.Empty() {
		// Still count a selected-but-unbacked behavior (failed asset check).
		r.mgr.PrepareBehavior(r.cfg, "")
		return fwdBody, kiro
	}
	got := r.mgr.PrepareBehavior(r.cfg, tokenopt.SystemText(proto, body))
	if got.Empty() {
		return fwdBody, kiro
	}
	if route, targets := config.ResolveRoute(model); route != nil && len(targets) > 0 {
		out, err := tokenopt.InjectSystem(proto, body, got.Text)
		if err != nil {
			r.mgr.NoteInjectFailed()
			logger.Warnf("[TokenOpt] could not append the behavior prompt (%v); relaying the request as written", err)
			return fwdBody, kiro
		}
		fwdBody = out
	}
	return fwdBody, kiro
}

// compressOpenAIMessages runs input compression over messages that are already
// parsed (the Responses-to-Kiro path assembles them from stored history plus the
// new input). They are rendered as a chat body, compressed, and read back; any
// failure returns the originals.
func (r tokenOptRun) compressOpenAIMessages(ctx context.Context, msgs []OpenAIMessage, model string) []OpenAIMessage {
	if r.mgr == nil || len(msgs) == 0 {
		return msgs
	}
	raw, err := json.Marshal(map[string]any{"model": model, "messages": msgs})
	if err != nil {
		return msgs
	}
	res := r.input(ctx, tokenopt.ProtocolOpenAI, raw, model)
	if !res.Changed {
		return msgs
	}
	var back struct {
		Messages []OpenAIMessage `json:"messages"`
	}
	if err := json.Unmarshal(res.Body, &back); err != nil || len(back.Messages) != len(msgs) {
		return msgs
	}
	return back.Messages
}

// appendBehaviorPrompt returns prompt with the official block appended, unless
// the prompt already carries it (the client has Caveman or Ponytail installed)
// or there is nothing to add. The block goes last: the client's own text and the
// thinking priming keep their positions.
func appendBehaviorPrompt(prompt string, b tokenopt.Behavior) string {
	if b.Empty() {
		return prompt
	}
	b = b.WithoutPresent(prompt)
	if b.Empty() {
		return prompt
	}
	if strings.TrimSpace(prompt) == "" {
		return b.Text
	}
	return strings.TrimRight(prompt, " \t\r\n") + "\n\n" + b.Text
}

// behaviorTokens is the estimated size of the block, added to a request's input
// estimate so accounting reflects what is actually sent. Zero when nothing is
// injected, which is what keeps accounting unchanged while the feature is off.
func behaviorTokens(b tokenopt.Behavior) int {
	if b.Empty() {
		return 0
	}
	return estimateApproxTokens(b.Text)
}
