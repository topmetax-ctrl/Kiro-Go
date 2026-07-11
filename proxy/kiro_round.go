package proxy

import (
	"context"
	"encoding/json"

	"kiro-go/config"
)

// KiroRoundEventKind tags an ordered event captured from one Kiro round so the
// stream handler can replay the final round in the exact order it arrived.
type KiroRoundEventKind int

const (
	RoundEventText KiroRoundEventKind = iota
	RoundEventThinking
	RoundEventToolUse
)

// KiroRoundEvent is one ordered piece of a round's output.
type KiroRoundEvent struct {
	Kind    KiroRoundEventKind
	Text    string
	ToolUse *KiroToolUse
}

// KiroRoundResult is the buffered outcome of a single Kiro call. The runner
// inspects ToolUses to decide whether to continue; the handler replays Events
// (final round only) or reads VisibleContent/ThinkingContent (non-stream).
type KiroRoundResult struct {
	Events          []KiroRoundEvent
	VisibleContent  string
	ThinkingContent string
	ToolUses        []KiroToolUse
	InputTokens     int
	OutputTokens    int
	Credits         float64
	ContextUsagePct float64
}

// KiroRoundCaller performs one buffered Kiro round. It is the single seam the
// runner depends on; tests supply a fake, production wraps CallKiroAPIContext.
type KiroRoundCaller interface {
	CallRound(ctx context.Context, account *config.Account, payload *KiroPayload) (KiroRoundResult, error)
}

// liveKiroRoundCaller is the production KiroRoundCaller. It drives
// CallKiroAPIContext with a capture callback that records ordered events plus
// aggregated content/tool-use/usage for one round.
type liveKiroRoundCaller struct{}

// NewKiroRoundCaller returns the production round caller.
func NewKiroRoundCaller() KiroRoundCaller { return liveKiroRoundCaller{} }

func (liveKiroRoundCaller) CallRound(ctx context.Context, account *config.Account, payload *KiroPayload) (KiroRoundResult, error) {
	var res KiroRoundResult
	cb := &KiroStreamCallback{
		OnText: func(text string, isThinking bool) {
			if text == "" {
				return
			}
			if isThinking {
				res.ThinkingContent += text
				res.Events = append(res.Events, KiroRoundEvent{Kind: RoundEventThinking, Text: text})
			} else {
				res.VisibleContent += text
				res.Events = append(res.Events, KiroRoundEvent{Kind: RoundEventText, Text: text})
			}
		},
		OnToolUse: func(tu KiroToolUse) {
			// Copy so the &-taken address is stable per event.
			cp := tu
			res.ToolUses = append(res.ToolUses, cp)
			res.Events = append(res.Events, KiroRoundEvent{Kind: RoundEventToolUse, ToolUse: &cp})
		},
		OnComplete: func(inTok, outTok int) {
			res.InputTokens = inTok
			res.OutputTokens = outTok
		},
		OnCredits: func(c float64) {
			res.Credits = c
		},
		OnContextUsage: func(pct float64) {
			res.ContextUsagePct = pct
		},
	}
	if err := CallKiroAPIContext(ctx, account, payload, cb); err != nil {
		return KiroRoundResult{}, err
	}
	return res, nil
}

// cloneKiroPayload returns a deep copy of a payload safe to mutate across
// rounds without touching the handler's original. The serializable graph is
// duplicated via a JSON round-trip; ToolNameMap (json:"-") is copied
// explicitly since it would otherwise be lost.
func cloneKiroPayload(src *KiroPayload) *KiroPayload {
	if src == nil {
		return nil
	}
	data, err := json.Marshal(src)
	if err != nil {
		// A payload that fails to marshal cannot be sent anyway; fall back to a
		// shallow copy so the caller still gets a non-nil, independent header.
		cp := *src
		return &cp
	}
	var dst KiroPayload
	if err := json.Unmarshal(data, &dst); err != nil {
		cp := *src
		return &cp
	}
	if len(src.ToolNameMap) > 0 {
		dst.ToolNameMap = make(map[string]string, len(src.ToolNameMap))
		for k, v := range src.ToolNameMap {
			dst.ToolNameMap[k] = v
		}
	}
	return &dst
}
