package cachetrace

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sync"
	"testing"

	"kiro-go/metrics"
)

const (
	parentID = "0a1b2c3d-0000-4000-8000-00000000abcd"
	childID  = "9f8e7d6c-0000-4000-8000-00000000dcba"
)

func with(kv map[string]any) func(map[string]any) {
	return func(r map[string]any) {
		for k, v := range kv {
			if v == nil {
				delete(r, k)
			} else {
				r[k] = v
			}
		}
	}
}

func signals(t *testing.T, mutate func(map[string]any), in Request) *metrics.CacheSignals {
	t.Helper()
	tr := testTracer.TraceFor(body(t, turns(3), 2, mutate), "", in)
	if tr == nil || tr.Signals == nil {
		t.Fatal("no signals")
	}
	return tr.Signals
}

func text(s string) []any { return []any{map[string]any{"type": "text", "text": s}} }

// Without headers or a session parser the trace can read the body and nothing
// else, and says so instead of saying "absent".
func TestSignalsWithoutRequestContextAreNotObservable(t *testing.T) {
	tr := testTracer.Trace(body(t, turns(3), 2, nil), "")
	got := *tr.Signals
	want := metrics.CacheSignals{
		ParentSession: metrics.SignalNotObservable, AgentHeader: metrics.SignalNotObservable,
		ExtendedTTLBeta: metrics.SignalNotObservable, SubagentMarker: metrics.SignalAbsent,
		Probe: metrics.SignalAbsent, TitleHelper: metrics.SignalAbsent,
	}
	if got != want {
		t.Fatalf("signals = %+v, want %+v", got, want)
	}
	if tr.Sess != "" || tr.Parent != "" {
		t.Fatalf("a session digest without a parser: %q %q", tr.Sess, tr.Parent)
	}
}

func TestIngressSignals(t *testing.T) {
	for name, tc := range map[string]struct {
		in          *Ingress
		agent, beta string
	}{
		"headers unavailable": {nil, "not-observable", "not-observable"},
		"plain request":       {&Ingress{}, "absent", "absent"},
		"agent header":        {&Ingress{AgentID: true}, "present", "absent"},
		"other beta only":     {&Ingress{Beta: "claude-code-20250219,interleaved-thinking-2025-05-14"}, "absent", "absent"},
		"extended ttl beta":   {&Ingress{Beta: "claude-code-20250219," + extendedTTLBeta}, "absent", "present"},
		"both":                {&Ingress{AgentID: true, Beta: extendedTTLBeta}, "present", "present"},
	} {
		got := signals(t, nil, Request{Ingress: tc.in})
		if got.AgentHeader != tc.agent || got.ExtendedTTLBeta != tc.beta {
			t.Errorf("%s: agent %s beta %s, want %s %s", name, got.AgentHeader, got.ExtendedTTLBeta, tc.agent, tc.beta)
		}
	}
}

// Only system[0] (or a string system) is read for the marker, as the upstream
// hop does: the same words in a later block or in a message are not it.
func TestSubagentMarker(t *testing.T) {
	billing := "x-anthropic-billing-header: cc_version=1.2.3; cc_entrypoint=cli; cc_is_subagent=true;"
	for name, tc := range map[string]struct {
		mutate func(map[string]any)
		want   string
	}{
		"first block":        {with(map[string]any{"system": text(billing)}), "present"},
		"string system":      {with(map[string]any{"system": billing}), "present"},
		"second block only":  {with(map[string]any{"system": []any{text("main")[0], text(billing)[0]}}), "absent"},
		"inside a message":   {with(map[string]any{"messages": []any{map[string]any{"role": "user", "content": billing}}}), "absent"},
		"other tag":          {with(map[string]any{"system": text("cc_is_subagent=false;")}), "absent"},
		"no system":          {with(map[string]any{"system": nil}), "absent"},
		"block without text": {with(map[string]any{"system": []any{map[string]any{"type": "image"}}}), "absent"},
		"system of a number": {with(map[string]any{"system": 7}), "absent"},
	} {
		if got := signals(t, tc.mutate, Request{}).SubagentMarker; got != tc.want {
			t.Errorf("%s: %s, want %s", name, got, tc.want)
		}
	}
}

func TestProbe(t *testing.T) {
	user := func(content any) []any { return []any{map[string]any{"role": "user", "content": content}} }
	for name, tc := range map[string]struct {
		kv   map[string]any
		want string
	}{
		"quota string":        {map[string]any{"max_tokens": 1, "tools": nil, "messages": user("quota")}, "present"},
		"padded probe":        {map[string]any{"max_tokens": 1, "tools": nil, "messages": user("  test \n")}, "present"},
		"block with reminder": {map[string]any{"max_tokens": 1, "tools": nil, "messages": user([]any{text("<system-reminder>x</system-reminder>")[0], text(".")[0]})}, "present"},
		"Hi with a marker": {map[string]any{"max_tokens": 1, "tools": nil, "messages": user([]any{
			map[string]any{"type": "text", "text": "Hi", "cache_control": map[string]any{"type": "ephemeral"}}})}, "present"},
		"Hi without marker":   {map[string]any{"max_tokens": 1, "tools": nil, "messages": user([]any{text("Hi")[0]})}, "absent"},
		"more than one token": {map[string]any{"max_tokens": 2, "tools": nil, "messages": user("quota")}, "absent"},
		"no max_tokens":       {map[string]any{"max_tokens": nil, "tools": nil, "messages": user("quota")}, "absent"},
		"offers tools":        {map[string]any{"max_tokens": 1, "messages": user("quota")}, "absent"},
		"empty tools":         {map[string]any{"max_tokens": 1, "tools": []any{}, "messages": user("quota")}, "present"},
		"ordinary text":       {map[string]any{"max_tokens": 1, "tools": nil, "messages": user("fix the bug")}, "absent"},
		"two plain blocks":    {map[string]any{"max_tokens": 1, "tools": nil, "messages": user([]any{text("quota")[0], text("more")[0]})}, "absent"},
		"assistant message": {map[string]any{"max_tokens": 1, "tools": nil, "messages": []any{
			map[string]any{"role": "assistant", "content": "quota"}}}, "absent"},
		"two messages": {map[string]any{"max_tokens": 1, "tools": nil, "messages": []any{
			map[string]any{"role": "user", "content": "quota"}, map[string]any{"role": "assistant", "content": "x"}}}, "absent"},
	} {
		if got := signals(t, with(tc.kv), Request{}).Probe; got != tc.want {
			t.Errorf("%s: %s, want %s", name, got, tc.want)
		}
	}
}

func TestTitleHelper(t *testing.T) {
	schema := func(props map[string]any) map[string]any {
		return map[string]any{"format": map[string]any{"type": "json_schema", "schema": map[string]any{"properties": props}}}
	}
	title := map[string]any{"title": map[string]any{"type": "string"}}
	ask := []any{map[string]any{"role": "user", "content": text("Return a short title for: <session>x</session>")}}
	for name, tc := range map[string]struct {
		kv   map[string]any
		want string
	}{
		"system names the task":    {map[string]any{"system": text("You are naming a coding session.")}, "present"},
		"system asks for a title":  {map[string]any{"system": "Return a short title"}, "present"},
		"language rule":            {map[string]any{"system": []any{text("main")[0], text("Write the title in the predominant language")[0]}}, "present"},
		"ordinary system":          {nil, "absent"},
		"instruction in a message": {map[string]any{"messages": ask}, "absent"},
		"title schema and message": {map[string]any{"output_config": schema(title), "messages": ask}, "present"},
		"title schema, session tag in a string message": {map[string]any{"output_config": schema(title),
			"messages": []any{map[string]any{"role": "user", "content": "<session>x</session>"}}}, "present"},
		"title schema and no instruction": {map[string]any{"output_config": schema(title)}, "absent"},
		"schema with more properties": {map[string]any{"system": text("Return a short title"),
			"output_config": schema(map[string]any{"title": 1, "body": 1})}, "absent"},
		"schema of another property": {map[string]any{"system": text("Return a short title"),
			"output_config": schema(map[string]any{"summary": 1})}, "absent"},
		"session tag only counts with a schema": {map[string]any{"system": text("<session>")}, "absent"},
	} {
		if got := signals(t, with(tc.kv), Request{}).TitleHelper; got != tc.want {
			t.Errorf("%s: %s, want %s", name, got, tc.want)
		}
	}
}

// identify returns the same identity for every body and records what it was given.
func identify(id Identity, seen *[]json.RawMessage) func(json.RawMessage) Identity {
	return func(raw json.RawMessage) Identity {
		if seen != nil {
			*seen = append(*seen, raw)
		}
		return id
	}
}

func TestIdentifyReceivesTheRawUserID(t *testing.T) {
	var seen []json.RawMessage
	in := Request{Identify: identify(Identity{Shape: "native", Parent: metrics.SignalNotObservable}, &seen)}
	for _, md := range []any{
		map[string]any{"user_id": "user_x"},
		map[string]any{"user_id": map[string]any{"parent_session_id": "p"}},
		map[string]any{},
		"not an object",
		nil,
	} {
		testTracer.TraceFor(body(t, turns(2), -1, with(map[string]any{"metadata": md})), "", in)
	}
	want := []string{`"user_x"`, `{"parent_session_id":"p"}`, "", "", ""}
	if len(seen) != len(want) {
		t.Fatalf("Identify called %d times, want %d", len(seen), len(want))
	}
	for i, w := range want {
		if string(seen[i]) != w {
			t.Errorf("call %d got %q, want %q", i, seen[i], w)
		}
	}
}

func TestIdentityFillsTheSessionSignals(t *testing.T) {
	got := signals(t, nil, Request{Identify: identify(Identity{Shape: "json", Parent: metrics.SignalPresent}, nil)})
	if got.UserID != "json" || got.ParentSession != metrics.SignalPresent {
		t.Fatalf("signals = %+v", got)
	}
}

func parentChild(t *testing.T, principalParent, principalChild string) (parent, child *metrics.CacheTrace) {
	t.Helper()
	b := body(t, turns(3), 2, nil)
	parent = testTracer.TraceFor(b, "", Request{Principal: principalParent, Identify: identify(Identity{Session: parentID}, nil)})
	child = testTracer.TraceFor(b, "", Request{Principal: principalChild, Identify: identify(Identity{Session: childID, ParentSession: parentID}, nil)})
	return parent, child
}

// The point of the fingerprints: a child's parent digest equals the parent's own
// session digest, and neither shows the id.
func TestParentDigestMatchesTheParentSession(t *testing.T) {
	parent, child := parentChild(t, "key-1", "key-1")
	if parent.Sess == "" || len(parent.Sess) != digestBytes*2 {
		t.Fatalf("parent digest %q", parent.Sess)
	}
	if child.Parent != parent.Sess {
		t.Fatalf("child.Parent %q != parent.Sess %q", child.Parent, parent.Sess)
	}
	if child.Sess == parent.Sess || child.Sess == "" {
		t.Fatalf("child and parent must be different sessions: %q %q", child.Sess, parent.Sess)
	}
	if parent.Parent != "" {
		t.Fatalf("a request with no parent has a parent digest %q", parent.Parent)
	}
	raw, _ := json.Marshal(child)
	for _, secret := range []string{parentID, childID} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("trace holds a raw session id: %s", raw)
		}
	}
}

// Two principals sending the same session id get unrelated digests, so a digest
// cannot be used to find one person's session in another's events.
func TestSessionDigestsAreScopedToThePrincipal(t *testing.T) {
	parent, child := parentChild(t, "key-1", "key-2")
	if child.Parent == parent.Sess {
		t.Fatal("a child under another principal linked to the parent")
	}
	other := testTracer.TraceFor(body(t, turns(3), 2, nil), "", Request{Principal: "key-2", Identify: identify(Identity{Session: parentID}, nil)})
	if other.Sess == parent.Sess {
		t.Fatal("one session id under two principals gave one digest")
	}
	none := testTracer.TraceFor(body(t, turns(3), 2, nil), "", Request{Identify: identify(Identity{Session: parentID}, nil)})
	if none.Sess == parent.Sess || none.Sess == other.Sess {
		t.Fatal("no principal must be a scope of its own")
	}
}

// A principal and an id are framed, so moving bytes between them changes the digest.
func TestSessionDigestFramesPrincipalAndID(t *testing.T) {
	if testTracer.sessionDigest("ab", "c") == testTracer.sessionDigest("a", "bc") {
		t.Fatal("principal and id run together")
	}
}

func TestSessionKeyIsSeparateFromTheContentKey(t *testing.T) {
	if bytes.Equal(testTracer.key, testTracer.sessKey) {
		t.Fatal("the session key is the content key")
	}
	content := short(testTracer.sum([]byte(""), []byte(parentID)))
	if testTracer.sessionDigest("", parentID) == content {
		t.Fatal("a session digest equals a content digest of the same bytes")
	}
	other := New([]byte("another-secret-another-secret-xx"))
	if other.sessionDigest("key-1", parentID) == testTracer.sessionDigest("key-1", parentID) {
		t.Fatal("a different secret must give a different session digest")
	}
}

func TestSessionDigestIgnoresUUIDCase(t *testing.T) {
	upper := "0A1B2C3D-0000-4000-8000-00000000ABCD"
	if testTracer.sessionDigest("k", upper) != testTracer.sessionDigest("k", parentID) {
		t.Fatal("one UUID in two cases made two sessions")
	}
	// Anything that is not a canonical UUID is used exactly as sent.
	if testTracer.sessionDigest("k", "Token-A") == testTracer.sessionDigest("k", "token-a") {
		t.Fatal("a token that is not a UUID was case folded")
	}
}

func TestUnknownSessionIsOmitted(t *testing.T) {
	for name, id := range map[string]Identity{
		"no ids":                 {Shape: "none"},
		"parent without session": {Shape: "json", Parent: metrics.SignalPresent},
	} {
		tr := testTracer.TraceFor(body(t, turns(2), -1, nil), "", Request{Principal: "k", Identify: identify(id, nil)})
		if tr.Sess != "" || tr.Parent != "" {
			t.Errorf("%s: invented a digest %q %q", name, tr.Sess, tr.Parent)
		}
	}
	if (&Tracer{key: []byte("k")}).sessionDigest("p", parentID) != "" {
		t.Fatal("a tracer with no session key made a digest")
	}
}

// Digests are recorded for days and compared across restarts and releases, so
// the derivation is pinned. An independent HMAC-SHA256 implementation reproduces
// these from the scheme in docs/cache-observability.md.
func TestSessionDigestsAreStableAcrossReleases(t *testing.T) {
	tr := New([]byte("pinned-secret-for-the-stability-test"))
	for _, tc := range []struct{ principal, id, want string }{
		{"key-1", parentID, "c4a2f09cafa1ac8fc516fc70"},
		{"", parentID, "a85d5920e245de50cea5ea82"},
		{"key-1", "plain-token", "386fda6598c150140b5927c0"},
	} {
		if got := tr.sessionDigest(tc.principal, tc.id); got != tc.want {
			t.Errorf("sessionDigest(%q, %q) = %s, want %s", tc.principal, tc.id, got, tc.want)
		}
	}
}

// Adding what the gateway knows about a request must not move a single content
// digest: every one recorded before this existed has to stay comparable.
func TestRequestContextDoesNotChangeContentDigests(t *testing.T) {
	b := body(t, turns(40), 39, nil)
	plain := testTracer.Trace(b, "p")
	full := testTracer.TraceFor(b, "p", Request{
		Principal: "key-1", Ingress: &Ingress{AgentID: true, Beta: extendedTTLBeta},
		Identify: identify(Identity{Shape: "json", Parent: metrics.SignalPresent, Session: childID, ParentSession: parentID}, nil),
	})
	a, c := *plain, *full
	a.Signals, a.Sess, a.Parent, c.Signals, c.Sess, c.Parent = nil, "", "", nil, "", ""
	if !reflect.DeepEqual(a, c) {
		t.Fatalf("content digests moved:\n%+v\n%+v", a, c)
	}
}

func TestTraceForNeverChangesTheBody(t *testing.T) {
	b := body(t, turns(6), 5, with(map[string]any{"metadata": map[string]any{"user_id": "x"}}))
	before := append([]byte(nil), b...)
	testTracer.TraceFor(b, "", Request{Principal: "k", Ingress: &Ingress{}, Identify: identify(Identity{Session: childID}, nil)})
	if !bytes.Equal(b, before) {
		t.Fatal("the body changed")
	}
}

func TestTraceForIsSafeConcurrently(t *testing.T) {
	b := body(t, turns(30), 29, nil)
	in := Request{Principal: "key-1", Ingress: &Ingress{Beta: extendedTTLBeta},
		Identify: identify(Identity{Shape: "json", Parent: metrics.SignalPresent, Session: childID, ParentSession: parentID}, nil)}
	want, _ := json.Marshal(testTracer.TraceFor(b, "p", in))
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if got, _ := json.Marshal(testTracer.TraceFor(b, "p", in)); !bytes.Equal(got, want) {
					t.Errorf("traces differ under concurrency:\n%s\n%s", got, want)
					return
				}
			}
		}()
	}
	wg.Wait()
}
