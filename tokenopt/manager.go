package tokenopt

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"kiro-go/config"
	"kiro-go/logger"
)

// Manager is the single entry point the proxy uses. It holds the long-lived
// pieces (HTTP client, memo, breaker, metrics) and takes the settings snapshot
// as an argument on every call, so a settings change applies to the next
// request with no restart and no shared mutable configuration.
type Manager struct {
	estimate func(string) int
	memo     *memo
	br       *breaker
	mx       *Metrics
	obs      chan struct{} // bounds concurrent observe-mode samples
	ctx      context.Context
	cancel   context.CancelFunc

	mu        sync.Mutex
	client    *HeadroomClient
	clientKey string
	health    *HealthReport // latest Headroom verification, active or passive
	rtkHealth *HealthReport
}

// NewManager builds a Manager. estimate approximates the token count of a text;
// pass the gateway's own estimator so figures agree with the rest of the stats.
func NewManager(estimate func(string) int) *Manager {
	if estimate == nil {
		estimate = func(s string) int { return (len(s) + 3) / 4 }
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		estimate: estimate,
		memo:     newMemo(4096, 64<<20),
		br:       newBreaker(3, 30*time.Second),
		mx:       newMetrics(),
		obs:      make(chan struct{}, 2),
		ctx:      ctx,
		cancel:   cancel,
	}
}

// Close stops background observe samples.
func (m *Manager) Close() { m.cancel() }

func (m *Manager) clientFor(h config.HeadroomConfig) (*HeadroomClient, error) {
	key := h.BaseURL + "\x00" + h.APIKey + "\x00" + strconv.Itoa(h.TimeoutMs)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.client != nil && m.clientKey == key {
		return m.client, nil
	}
	c, err := NewHeadroomClient(h)
	if err != nil {
		return nil, err
	}
	if m.client != nil {
		m.client.hc.CloseIdleConnections()
	}
	m.client, m.clientKey = c, key
	return c, nil
}

// InputRequest is a request body offered for input compression.
type InputRequest struct {
	Protocol Protocol
	Body     []byte
	Model    string
	// Principal scopes the memo to a caller (API key id, or a shared anonymous name).
	Principal string
}

// InputResult is what to send upstream instead of the original body.
type InputResult struct {
	// Body is the body to send. It is the original, untouched, unless Changed.
	Body    []byte
	Changed bool
	// Outcome and Reason describe the decision, for logs and tests.
	Outcome string
	Reason  string
}

// candidates returns the slots worth sending to an engine: large enough to repay
// the round-trip, and not produced by a tool whose bytes must stay exact.
func candidates(d *document, minBytes int) []int {
	var out []int
	for i, s := range d.slots {
		if len(s.text) >= minBytes && !exactBytesTool(s.tool) {
			out = append(out, i)
		}
	}
	return out
}

// OptimizeInput applies the selected input-compression engine to a request.
//
// It never returns an error and never panics out: on any failure the original
// body comes back unchanged, so inference proceeds exactly as it would without
// the feature. cfg must be a resolved snapshot.
func (m *Manager) OptimizeInput(ctx context.Context, cfg config.TokenOptimizationConfig, in InputRequest) (res InputResult) {
	res = InputResult{Body: in.Body}
	// RTK is deliberately a no-op here: it has no gateway-level compress API
	// (see docs/token-optimization.md), so there is nothing for the gateway to do.
	if cfg.Mode == config.TokenOptModeOff || cfg.InputCompression != config.TokenOptInputHeadroom {
		return res
	}
	ev := Event{Engine: EngineHeadroom, Protocol: string(in.Protocol)}
	defer func() {
		if r := recover(); r != nil {
			// A half-open trial permit may be held at this point; hand it back or the
			// breaker would never let another call through.
			m.br.release()
			logger.Errorf("[TokenOpt] input optimization panicked; request sent unchanged: %v", r)
			ev.Outcome, ev.Reason = OutcomeError, "panic"
			m.mx.record(ev)
			res = InputResult{Body: in.Body, Outcome: OutcomeError, Reason: "panic"}
		}
	}()
	done := func(outcome, reason string) InputResult {
		ev.Outcome, ev.Reason = outcome, reason
		res.Outcome, res.Reason = outcome, reason
		if outcome == OutcomeSkipped && reason == ReasonNoCandidates {
			m.mx.count(ev.Engine, outcome, reason) // routine; keep it out of the recent list
		} else {
			m.mx.record(ev)
		}
		return res
	}

	if !mightHaveToolResults(in.Protocol, in.Body) {
		return done(OutcomeSkipped, ReasonNoCandidates)
	}
	client, err := m.clientFor(cfg.Headroom)
	if err != nil {
		return done(OutcomeSkipped, ReasonUnconfigured)
	}
	doc, err := parseDocument(in.Protocol, in.Body)
	if err != nil {
		return done(OutcomeSkipped, ReasonUnparseable)
	}
	cands := candidates(doc, cfg.Headroom.MinBytes)
	ev.Candidates = len(cands)
	if len(cands) == 0 {
		return done(OutcomeSkipped, ReasonNoCandidates)
	}
	if cfg.Mode == config.TokenOptModeObserve {
		m.observe(cfg, client, doc, in, cands)
		res.Outcome = "observing"
		return res
	}

	// --- on: replay what is already known, then ask the engine about the rest ---
	fp := "headroom/v1/" + cfg.Headroom.Mode
	keys := make(map[int][32]byte, len(cands))
	replay := map[int]string{}
	var pending []int
	for _, si := range cands {
		k := memoKey(in.Principal, fp, doc.slots[si].text)
		keys[si] = k
		if v, ok := m.memo.get(k); ok {
			ev.FromMemo++
			if v != doc.slots[si].text {
				replay[si] = v
			}
		} else {
			pending = append(pending, si)
		}
	}

	working := doc
	if len(replay) > 0 {
		nb, err := doc.apply(replay)
		var nd *document
		if err == nil {
			nd, err = parseDocument(in.Protocol, nb)
		}
		if err != nil || len(nd.slots) != len(doc.slots) {
			return done(OutcomeSkipped, ReasonUnparseable)
		}
		working, res.Body, res.Changed = nd, nb, true
		for si, v := range replay {
			old := doc.slots[si].text
			ev.Replaced++
			ev.BytesBefore += len(old)
			ev.BytesAfter += len(v)
			ev.EstTokensSaved += m.estimate(old) - m.estimate(v)
		}
	}
	// memoOnly ends the call with whatever the replay achieved.
	memoOnly := func(skipReason string) InputResult {
		if skipReason != "" {
			m.mx.count(EngineHeadroom, OutcomeSkipped, skipReason)
		}
		if res.Changed {
			return done(OutcomeApplied, ReasonFromMemo)
		}
		return done(OutcomeSkipped, skipReasonOr(skipReason, ReasonNoGain))
	}
	if len(pending) == 0 {
		return memoOnly("")
	}

	if !m.br.allow() {
		return memoOnly(ReasonCircuitOpen)
	}
	msgs, err := working.engineMessages()
	if err != nil {
		m.br.release()
		return memoOnly(ReasonUnparseable)
	}
	if len(msgs) > MaxEnginePayload {
		m.br.release()
		return memoOnly(ReasonTooLarge)
	}
	frozen := 0
	if cfg.Headroom.Mode == config.HeadroomModeCache {
		frozen = working.slots[pending[0]].viewMsg
		for _, si := range pending {
			frozen = min(frozen, working.slots[si].viewMsg)
		}
	}

	t0 := time.Now()
	reply, err := client.Compress(ctx, CompressParams{Model: in.Model, Messages: msgs, Mode: cfg.Headroom.Mode, Frozen: frozen})
	ev.DurationMs = max(1, time.Since(t0).Milliseconds())
	if err != nil {
		return m.engineFailed(ctx, err, &ev, &res, memoOnly)
	}
	texts, err := working.extract(reply.Messages, pending)
	if err != nil {
		return m.engineFailed(ctx, &EngineError{Kind: KindStructure, Err: err}, &ev, &res, memoOnly)
	}
	m.br.success()
	m.noteHealth(true, "")

	adopted := map[int]string{}
	for _, si := range pending {
		old, nw := working.slots[si].text, texts[si]
		if adoptable(old, nw) {
			adopted[si] = nw
			m.memo.put(keys[si], nw)
			ev.Replaced++
			ev.BytesBefore += len(old)
			ev.BytesAfter += len(nw)
			ev.EstTokensSaved += m.estimate(old) - m.estimate(nw)
		} else if cfg.Headroom.Mode == config.HeadroomModeCache {
			// Settled: in cache mode this text stays as the client sent it, and
			// remembering that keeps it from changing under a cached prefix later.
			m.memo.put(keys[si], old)
		}
	}
	if len(adopted) == 0 {
		if reply.TokensSaved > 0 {
			m.mx.addPhantom() // claimed savings, but nothing usable came back
		}
		return memoOnly(ReasonNoGain)
	}
	nb, err := working.apply(adopted)
	if err != nil {
		return memoOnly(ReasonUnparseable)
	}
	ev.EngineTokensSaved = reply.TokensSaved
	res.Body, res.Changed = nb, true
	return done(OutcomeApplied, "")
}

func skipReasonOr(reason, fallback string) string {
	if reason != "" {
		return reason
	}
	return fallback
}

// engineFailed records a failed engine call and falls back to whatever the memo
// replay produced (or the untouched original).
func (m *Manager) engineFailed(ctx context.Context, err error, ev *Event, res *InputResult, memoOnly func(string) InputResult) InputResult {
	var ee *EngineError
	if !errors.As(err, &ee) {
		ee = &EngineError{Kind: KindUnavailable, Err: err}
	}
	// If the caller's own context ended (client hung up, or its deadline passed)
	// the failure belongs to the caller, whatever shape the error took: it says
	// nothing about the engine and must never count against its breaker.
	if ctx.Err() != nil || ee.Kind == KindCanceled {
		m.br.release()
		return memoOnly(ReasonCanceled)
	}
	m.br.failure()
	m.noteHealth(false, string(ee.Kind))
	logger.Warnf("[TokenOpt] headroom %s; sending the request without it", ee)
	ev.Outcome, ev.Reason = OutcomeError, string(ee.Kind)
	m.mx.record(*ev)
	res.Outcome, res.Reason = OutcomeError, string(ee.Kind)
	return *res
}

// observe samples what the engine would save without touching the request. The
// call runs in the background, bounded, so inference never waits on it; the
// compressed text is measured and discarded, and nothing different is sent to a
// provider. It does send the conversation to the configured engine, which is the
// service the operator pointed this at.
func (m *Manager) observe(cfg config.TokenOptimizationConfig, client *HeadroomClient, doc *document, in InputRequest, cands []int) {
	ev := Event{Engine: EngineHeadroom, Protocol: string(in.Protocol), Candidates: len(cands)}
	select {
	case m.obs <- struct{}{}:
	default:
		m.mx.count(EngineHeadroom, OutcomeSkipped, ReasonObserveBusy)
		return
	}
	if !m.br.allow() {
		<-m.obs
		m.mx.count(EngineHeadroom, OutcomeSkipped, ReasonCircuitOpen)
		return
	}
	go func() {
		defer func() { <-m.obs }()
		defer func() {
			if r := recover(); r != nil {
				m.br.release()
				logger.Errorf("[TokenOpt] observe panicked: %v", r)
			}
		}()
		msgs, err := doc.engineMessages()
		if err != nil || len(msgs) > MaxEnginePayload {
			m.br.release()
			m.mx.count(EngineHeadroom, OutcomeSkipped, ReasonTooLarge)
			return
		}
		t0 := time.Now()
		reply, err := client.Compress(m.ctx, CompressParams{Model: in.Model, Messages: msgs, Mode: cfg.Headroom.Mode})
		ev.DurationMs = max(1, time.Since(t0).Milliseconds())
		var texts map[int]string
		if err == nil {
			if texts, err = doc.extract(reply.Messages, cands); err != nil {
				err = &EngineError{Kind: KindStructure, Err: err}
			}
		}
		if err != nil {
			var ee *EngineError
			if m.ctx.Err() != nil || (errors.As(err, &ee) && ee.Kind == KindCanceled) {
				m.br.release()
				return
			}
			m.br.failure()
			kind := string(KindUnavailable)
			if ee != nil {
				kind = string(ee.Kind)
			}
			m.noteHealth(false, kind)
			ev.Outcome, ev.Reason = OutcomeError, kind
			m.mx.record(ev)
			return
		}
		m.br.success()
		m.noteHealth(true, "")
		for _, si := range cands {
			if old, nw := doc.slots[si].text, texts[si]; adoptable(old, nw) {
				ev.Replaced++
				ev.BytesBefore += len(old)
				ev.BytesAfter += len(nw)
				ev.EstTokensSaved += m.estimate(old) - m.estimate(nw)
			}
		}
		ev.EngineTokensSaved = reply.TokensSaved
		ev.Outcome = OutcomeObserved
		m.mx.record(ev)
	}()
}

// noteHealth keeps the latest passive health signal from real traffic, without
// discarding the version learned by an active probe.
func (m *Manager) noteHealth(ok bool, errKind string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h := HealthReport{CheckedAt: time.Now(), Reachable: ok, CompressOK: ok}
	if m.health != nil {
		h.Service, h.Version, h.Compat = m.health.Service, m.health.Version, m.health.Compat
	}
	if !ok {
		h.Reachable = errKind != string(KindUnavailable) && errKind != string(KindTimeout)
		h.Error = errKind
	}
	m.health = &h
}

// PrepareBehavior returns the official prompt block to apply to a request whose
// client-side system text is `system`, and counts the decision. It is empty
// unless the engine mode is "on".
func (m *Manager) PrepareBehavior(cfg config.TokenOptimizationConfig, system string) Behavior {
	want := m.WantedBehavior(cfg)
	if want.Empty() {
		if cfg.Mode == config.TokenOptModeOn && (cfg.Caveman != config.TokenOptStyleOff || cfg.Ponytail != config.TokenOptPonytailOff) {
			// Something was selected but no verified asset backs it.
			m.mx.count(EngineCaveman+"/"+EnginePonytail, OutcomeSkipped, ReasonAssetIntegrity)
		}
		return want
	}
	got := want.WithoutPresent(system)
	for _, p := range []struct {
		engine, asked, applied string
	}{
		{EngineCaveman, want.Caveman, got.Caveman},
		{EnginePonytail, want.Ponytail, got.Ponytail},
	} {
		switch {
		case p.asked == "":
		case p.applied != "":
			m.mx.count(p.engine, OutcomeApplied, "")
		default:
			m.mx.count(p.engine, OutcomeSkipped, ReasonAlreadyPresent)
		}
	}
	return got
}

// WantedBehavior is the block the settings select, before any per-request
// dedup. The Kiro-native translator dedups against its own final prompt, so it
// receives this form. It is a pure function of cfg: no I/O, no counting.
func (m *Manager) WantedBehavior(cfg config.TokenOptimizationConfig) Behavior {
	if cfg.Mode != config.TokenOptModeOn {
		return Behavior{}
	}
	return ComposeBehavior(cfg.Caveman, cfg.Ponytail)
}

// NoteInjectFailed counts a prompt injection that could not be applied.
func (m *Manager) NoteInjectFailed() {
	m.mx.count("behavior", OutcomeError, ReasonInjectFailed)
}

// --- verification ---

// ProviderRef is an upstream provider as far as RTK wiring detection cares.
type ProviderRef struct {
	Name    string
	BaseURL string
	Enabled bool
}

// VerifyHeadroom probes a Headroom service end to end. When persist is set the
// result becomes the stored "last check"; a probe of unsaved form values must
// not overwrite the status of the settings actually in force.
func (m *Manager) VerifyHeadroom(ctx context.Context, h config.HeadroomConfig, persist bool) HealthReport {
	c, err := NewHeadroomClient(h)
	if err != nil {
		return HealthReport{CheckedAt: time.Now(), Error: err.Error()}
	}
	defer c.hc.CloseIdleConnections()
	rep := c.Probe(ctx)
	if persist {
		m.mu.Lock()
		m.health = &rep
		m.mu.Unlock()
	}
	return rep
}

// VerifyRTK checks the configured rtk-proxy's /health. It can say whether the
// daemon is up; it cannot say anything about compression, which RTK does not
// expose.
func (m *Manager) VerifyRTK(ctx context.Context, proxyURL string, persist bool) HealthReport {
	rep := ProbeURL(ctx, proxyURL, 3*time.Second)
	if persist {
		m.mu.Lock()
		m.rtkHealth = &rep
		m.mu.Unlock()
	}
	return rep
}

// LastHealth returns the stored verification results.
func (m *Manager) LastHealth() (headroom, rtk *HealthReport) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.health != nil {
		h := *m.health
		headroom = &h
	}
	if m.rtkHealth != nil {
		r := *m.rtkHealth
		rtk = &r
	}
	return
}

// endpointKey reduces a URL to host:port with loopback spellings unified, so
// "localhost" and "127.0.0.1" compare equal.
func endpointKey(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || host == "::1" || host == "127.0.0.1" {
		host = "loopback"
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	return host + ":" + port
}

// RoutedProviders lists the enabled providers whose base URL points at proxyURL,
// i.e. the providers whose traffic really passes through an rtk-proxy.
func RoutedProviders(proxyURL string, providers []ProviderRef) []string {
	want := endpointKey(proxyURL)
	if want == "" {
		return []string{}
	}
	out := []string{}
	for _, p := range providers {
		if p.Enabled && endpointKey(p.BaseURL) == want {
			out = append(out, p.Name)
		}
	}
	return out
}
