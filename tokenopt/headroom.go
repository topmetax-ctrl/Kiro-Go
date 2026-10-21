package tokenopt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	"kiro-go/config"
)

// TestedHeadroomVersion is the Headroom release this adapter was verified
// against (request/response shapes read from its source at that tag). A service
// reporting another major.minor still works as long as the reply passes the
// structural checks; the admin view just marks it "untested".
const TestedHeadroomVersion = "0.41.0"

// MaxEnginePayload bounds what one call may send or accept. Larger requests are
// left alone rather than risking the engine's own body limit mid-flight.
const MaxEnginePayload = 8 << 20

// ErrKind classifies an engine failure. The set is small and fixed so it can be
// used as a metrics label without exploding cardinality.
type ErrKind string

const (
	KindTimeout     ErrKind = "timeout"
	KindUnavailable ErrKind = "unavailable"
	KindRejected    ErrKind = "rejected"
	KindServer      ErrKind = "server"
	KindBadResponse ErrKind = "bad_response"
	KindTooLarge    ErrKind = "too_large"
	KindStructure   ErrKind = "structure_mismatch"
	// KindCanceled is the caller giving up (client disconnected). It says nothing
	// about the engine's health and never counts against its circuit breaker.
	KindCanceled ErrKind = "canceled"
)

// EngineError is a classified failure of an external engine call. Its message
// never carries request or response content.
type EngineError struct {
	Kind   ErrKind
	Status int // HTTP status when one was received
	Err    error
}

func (e *EngineError) Error() string {
	msg := string(e.Kind)
	if e.Status != 0 {
		msg += " (HTTP " + strconv.Itoa(e.Status) + ")"
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}
func (e *EngineError) Unwrap() error { return e.Err }

// unsafeAddr reports addresses an engine call must never reach whatever the
// configured hostname says: link-local (which includes the 169.254.169.254
// cloud metadata service), multicast, the unspecified address, and the other
// well-known metadata endpoints. Loopback and private ranges stay allowed: the
// engine is meant to run beside the gateway.
func unsafeAddr(ip net.IP) bool {
	if ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true
	}
	return ip.Equal(net.ParseIP("100.100.100.200")) || ip.Equal(net.ParseIP("fd00:ec2::254"))
}

// blockUnsafeDial runs on every connection attempt with the address the
// resolver actually produced, so DNS rebinding cannot swap in a metadata IP
// after the URL was validated.
func blockUnsafeDial(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || unsafeAddr(ip) {
		return fmt.Errorf("refusing to connect to %s: link-local, multicast and metadata addresses are not allowed", host)
	}
	return nil
}

// newGuardedClient builds the HTTP client used for every optional-service call:
// no outbound proxy (the Kiro proxy setting is for Kiro traffic, not for a
// sidecar on localhost), no redirects (a 3xx from the service must not steer the
// request elsewhere), dial-time address filtering, and bounded header wait.
func newGuardedClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: (&net.Dialer{
				Timeout: 2 * time.Second,
				Control: blockUnsafeDial,
			}).DialContext,
			MaxIdleConns:          4,
			MaxIdleConnsPerHost:   4,
			IdleConnTimeout:       60 * time.Second,
			ResponseHeaderTimeout: timeout,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// HeadroomClient talks to a Headroom service (`headroom proxy`).
type HeadroomClient struct {
	base    string
	token   string
	timeout time.Duration
	hc      *http.Client
}

// NewHeadroomClient validates the configured URL and builds a client.
func NewHeadroomClient(cfg config.HeadroomConfig) (*HeadroomClient, error) {
	base, err := config.NormalizeServiceURL(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("headroom base URL: %w", err)
	}
	timeout := time.Duration(cfg.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = config.DefaultHeadroomTimeout * time.Millisecond
	}
	return &HeadroomClient{base: base, token: strings.TrimSpace(cfg.APIKey), timeout: timeout, hc: newGuardedClient(timeout)}, nil
}

// CompressParams is one /v1/compress call.
type CompressParams struct {
	Model    string
	Messages []byte // JSON array
	// Mode is config.HeadroomModeCache or HeadroomModeToken.
	Mode string
	// Frozen is the number of leading messages the engine must return untouched.
	Frozen int
}

// CompressReply is the part of Headroom's reply this adapter uses.
type CompressReply struct {
	Messages     []any // decoded with numbers preserved
	TokensBefore int
	TokensAfter  int
	TokensSaved  int
	Transforms   []string
}

func (c *HeadroomClient) newRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "kiro-go-tokenopt")
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("x-headroom-proxy-token", c.token)
	}
	return req, nil
}

// classify turns a transport error into an EngineError.
func classify(err error) *EngineError {
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		return &EngineError{Kind: KindTimeout, Err: errors.New("deadline exceeded")}
	case errors.Is(err, context.Canceled):
		return &EngineError{Kind: KindCanceled, Err: errors.New("canceled")}
	}
	// url.Error text includes the full URL; keep only the cause.
	var ue interface{ Unwrap() error }
	if errors.As(err, &ue) && ue.Unwrap() != nil {
		err = ue.Unwrap()
	}
	return &EngineError{Kind: KindUnavailable, Err: err}
}

// do sends req and returns the status and a size-capped body.
func (c *HeadroomClient) do(req *http.Request) (int, []byte, *EngineError) {
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, classify(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxEnginePayload+1))
	if err != nil {
		return resp.StatusCode, nil, classify(err)
	}
	if len(data) > MaxEnginePayload {
		return resp.StatusCode, nil, &EngineError{Kind: KindTooLarge, Status: resp.StatusCode}
	}
	return resp.StatusCode, data, nil
}

// Compress calls POST /v1/compress. It never sets mode "ccr" or a session id:
// the default pipeline is marker-free (the reply needs no retrieval round-trip,
// which a stateless gateway could not serve), and byte-stability across turns is
// handled by the caller's memo plus the frozen prefix.
func (c *HeadroomClient) Compress(ctx context.Context, p CompressParams) (*CompressReply, error) {
	if len(p.Messages) > MaxEnginePayload {
		return nil, &EngineError{Kind: KindTooLarge}
	}
	cfg := map[string]any{}
	if p.Mode == config.HeadroomModeCache {
		// Cache mode compresses the newest observation immediately (the engine's own
		// "coding" profile does the same) and freezes everything before it.
		cfg["protect_recent"] = 0
		if p.Frozen > 0 {
			cfg["frozen_message_count"] = p.Frozen
		}
	}
	reqBody := bytes.NewBuffer(nil)
	reqBody.WriteString(`{"model":`)
	model, _ := encodeJSON(p.Model)
	reqBody.Write(model)
	reqBody.WriteString(`,"messages":`)
	reqBody.Write(p.Messages)
	if len(cfg) > 0 {
		cb, _ := encodeJSON(cfg)
		reqBody.WriteString(`,"config":`)
		reqBody.Write(cb)
	}
	reqBody.WriteByte('}')

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := c.newRequest(ctx, http.MethodPost, "/v1/compress", reqBody.Bytes())
	if err != nil {
		return nil, &EngineError{Kind: KindUnavailable, Err: err}
	}
	status, data, eerr := c.do(req)
	if eerr != nil {
		return nil, eerr
	}
	switch {
	case status == http.StatusOK:
	case status >= 500 || status == http.StatusTooManyRequests:
		return nil, &EngineError{Kind: KindServer, Status: status}
	default:
		// 401/403: token wrong. 404: Headroom answers 404 to callers its loopback
		// guard rejects. 400: it did not like the payload. All are non-retryable.
		return nil, &EngineError{Kind: KindRejected, Status: status}
	}

	var wire struct {
		Messages     json.RawMessage `json:"messages"`
		TokensBefore int             `json:"tokens_before"`
		TokensAfter  int             `json:"tokens_after"`
		TokensSaved  int             `json:"tokens_saved"`
		Transforms   []string        `json:"transforms_applied"`
	}
	if err := json.Unmarshal(data, &wire); err != nil || len(wire.Messages) == 0 {
		return nil, &EngineError{Kind: KindBadResponse, Status: status, Err: errors.New("reply is not a compress result")}
	}
	node, err := decodeNumber(wire.Messages)
	msgs, ok := node.([]any)
	if err != nil || !ok {
		return nil, &EngineError{Kind: KindBadResponse, Status: status, Err: errors.New("messages is not an array")}
	}
	return &CompressReply{
		Messages: msgs, TokensBefore: wire.TokensBefore, TokensAfter: wire.TokensAfter,
		TokensSaved: wire.TokensSaved, Transforms: wire.Transforms,
	}, nil
}

// HealthReport is the outcome of one verification of a service.
type HealthReport struct {
	CheckedAt time.Time `json:"checkedAt"`
	// Reachable: the liveness endpoint answered 200.
	Reachable bool   `json:"reachable"`
	LatencyMs int64  `json:"latencyMs"`
	Service   string `json:"service,omitempty"`
	Version   string `json:"version,omitempty"`
	// Compat is "tested", "untested" or "unknown" against TestedHeadroomVersion.
	Compat string `json:"compat,omitempty"`
	// CompressOK: a real (tiny) compress call succeeded. Liveness alone does not
	// prove it: /livez needs no token and no loopback, /v1/compress needs both.
	CompressOK bool   `json:"compressOk"`
	Error      string `json:"error,omitempty"`
}

func versionCompat(v string) string {
	if v == "" {
		return "unknown"
	}
	major := func(s string) string {
		p := strings.SplitN(strings.TrimPrefix(s, "v"), ".", 3)
		if len(p) < 2 {
			return ""
		}
		return p[0] + "." + p[1]
	}
	if m := major(v); m == "" {
		return "unknown"
	} else if m == major(TestedHeadroomVersion) {
		return "tested"
	}
	return "untested"
}

// Probe verifies the service end to end: liveness (with version), then a real
// compress call on a one-line conversation.
func (c *HeadroomClient) Probe(ctx context.Context) HealthReport {
	start := time.Now()
	rep := HealthReport{CheckedAt: start}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	status, data := 0, []byte(nil)
	for _, path := range []string{"/livez", "/health"} { // /health for older releases
		req, err := c.newRequest(ctx, http.MethodGet, path, nil)
		if err != nil {
			rep.Error = err.Error()
			return rep
		}
		var eerr *EngineError
		if status, data, eerr = c.do(req); eerr != nil {
			rep.Error = eerr.Error()
			rep.LatencyMs = time.Since(start).Milliseconds()
			return rep
		}
		if status != http.StatusNotFound {
			break
		}
	}
	rep.LatencyMs = time.Since(start).Milliseconds()
	if status != http.StatusOK {
		rep.Error = fmt.Sprintf("liveness answered HTTP %d", status)
		return rep
	}
	rep.Reachable = true
	var live struct {
		Service string `json:"service"`
		Version string `json:"version"`
	}
	_ = json.Unmarshal(data, &live)
	rep.Service, rep.Version, rep.Compat = live.Service, live.Version, versionCompat(live.Version)

	probeBody := []byte(`[{"role":"user","content":"ping"}]`)
	if _, err := c.Compress(ctx, CompressParams{Model: "gpt-4o", Messages: probeBody, Mode: config.HeadroomModeToken}); err != nil {
		var ee *EngineError
		if errors.As(err, &ee) && ee.Kind == KindRejected {
			switch ee.Status {
			case http.StatusUnauthorized, http.StatusForbidden:
				rep.Error = "compress route rejected the token"
			case http.StatusNotFound:
				rep.Error = "compress route is not reachable from this host: Headroom only serves /v1/compress to loopback callers unless HEADROOM_COMPRESS_ALLOW_REMOTE=1 is set"
			default:
				rep.Error = err.Error()
			}
		} else {
			rep.Error = err.Error()
		}
		return rep
	}
	rep.CompressOK = true
	return rep
}

// ProbeURL checks a plain HTTP health endpoint (used for rtk-proxy, which only
// documents GET /health). It reports reachability and nothing else: RTK Proxy
// exposes no version or compress API to verify further.
func ProbeURL(ctx context.Context, base string, timeout time.Duration) HealthReport {
	start := time.Now()
	rep := HealthReport{CheckedAt: start}
	u, err := config.NormalizeServiceURL(base)
	if err != nil {
		rep.Error = err.Error()
		return rep
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u+"/health", nil)
	if err != nil {
		rep.Error = err.Error()
		return rep
	}
	req.Header.Set("User-Agent", "kiro-go-tokenopt")
	resp, err := newGuardedClient(timeout).Do(req)
	rep.LatencyMs = time.Since(start).Milliseconds()
	if err != nil {
		rep.Error = classify(err).Error()
		return rep
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		rep.Error = fmt.Sprintf("health answered HTTP %d", resp.StatusCode)
		return rep
	}
	rep.Reachable = true
	return rep
}
