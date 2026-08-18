// Package proxy is the core proxy layer for the Kiro API.
// It handles streaming API calls to the Kiro backend and parses AWS Event Stream responses.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

const (
	// streamRetryBackoff spaces out a retry of a stream that died before
	// delivering any output callback. Upstream drops cluster in time, so an
	// immediate retry tends to hit the same blip.
	streamRetryBackoff           = 700 * time.Millisecond
	maxStreamAttemptsPerEndpoint = 2
	maxEventStreamMessageSize    = 16 * 1024 * 1024
)

var (
	errEmptyKiroStream         = errors.New("upstream stream ended before any output")
	errIncompleteKiroToolInput = errors.New("upstream stream ended with incomplete tool input")
	errInvalidKiroEventStream  = errors.New("invalid upstream event stream")
	errKiroEventStreamUpstream = errors.New("upstream event stream error")
	streamRetryWait            = waitForStreamRetry
	resolveKiroEndpoints       = endpointsForAccount
)

// Endpoint configuration (auto-fallback on quota exhaustion).
type kiroEndpoint struct {
	URL       string
	Origin    string
	AmzTarget string
	Name      string
}

var kiroEndpoints = []kiroEndpoint{
	{
		URL:       "https://q.us-east-1.amazonaws.com/generateAssistantResponse",
		Origin:    "AI_EDITOR",
		AmzTarget: "",
		Name:      "Kiro IDE",
	},
	{
		URL:       "https://codewhisperer.us-east-1.amazonaws.com/generateAssistantResponse",
		Origin:    "AI_EDITOR",
		AmzTarget: "AmazonCodeWhispererStreamingService.GenerateAssistantResponse",
		Name:      "CodeWhisperer",
	},
	{
		URL:       "https://q.us-east-1.amazonaws.com/generateAssistantResponse",
		Origin:    "AI_EDITOR",
		AmzTarget: "AmazonQDeveloperStreamingService.SendMessage",
		Name:      "AmazonQ",
	},
}

// kiroCLIEndpoint is the headless / API Key path used by Kiro CLI:
// POST https://runtime.{region}.kiro.dev/ with AWS JSON 1.0 protocol.
var kiroCLIEndpoint = kiroEndpoint{
	URL:       "https://runtime.us-east-1.kiro.dev/",
	Origin:    "KIRO_CLI",
	AmzTarget: "AmazonCodeWhispererStreamingService.GenerateAssistantResponse",
	Name:      "Kiro CLI",
}

// Global HTTP clients, swappable at runtime to apply proxy reconfiguration without restart.
var kiroHttpStore atomic.Pointer[http.Client]
var kiroRestHttpStore atomic.Pointer[http.Client]

// proxyClientCache caches http.Client instances keyed by proxy URL for per-account proxy support.
var proxyClientCache sync.Map

// NOTE: the account-injector branch shipped a utls browser-TLS fingerprint here
// (Safari/Chrome ClientHello via DialTLSContext) to dodge AWS anti-abuse. It is
// deliberately NOT carried over: installing a custom TLS dialer replaces the
// transport's Dial/TLSHandshake/ResponseHeader timeouts, which are what keep a
// long-lived SSE stream from either hanging on connection setup or being capped
// by a blanket client timeout, and it forces HTTP/1.1 because Go cannot detect
// ALPN-negotiated h2 from an external dialer. The branch's own history also shows
// utls added, reverted ("accounts are subscription-revoked, not TLS-blocked"),
// then re-applied. The inter-request jitter from the same effort IS kept — it is
// independent of the TLS layer. kiro_tls_terminator.py remains in the repo as a
// standalone alternative for anyone who needs a real browser TLS stack.

func init() {
	InitKiroHttpClient("")
}

// GetClientForProxy returns an http.Client configured for the given proxy URL.
// If proxyURL is empty, returns the global kiro HTTP client.
func GetClientForProxy(proxyURL string) *http.Client {
	if proxyURL == "" {
		return kiroHttpStore.Load()
	}
	if cached, ok := proxyClientCache.Load(proxyURL); ok {
		return cached.(*http.Client)
	}
	client := &http.Client{
		Timeout:   5 * time.Minute,
		Transport: buildKiroTransport(proxyURL),
	}
	proxyClientCache.Store(proxyURL, client)
	return client
}

// GetForwardClientForProxy returns an http.Client for upstream forwarding. Unlike
// GetClientForProxy it sets NO blanket Client.Timeout: a forwarded SSE stream can
// run for many minutes, and a total timeout would truncate a valid long stream.
// Connection setup and time-to-first-byte are bounded by the transport
// (Dial/TLSHandshake/ResponseHeader timeouts); overall lifetime is bounded by the
// request context (client disconnect cancels it).
func GetForwardClientForProxy(proxyURL string) *http.Client {
	cacheKey := "forward:" + proxyURL
	if cached, ok := proxyClientCache.Load(cacheKey); ok {
		return cached.(*http.Client)
	}
	client := &http.Client{
		// No Timeout on purpose (streaming-safe). Transport timeouts + request
		// context govern liveness.
		Transport: buildKiroTransport(proxyURL),
	}
	proxyClientCache.Store(cacheKey, client)
	return client
}

// GetRestClientForProxy returns a rest http.Client (30s timeout) for the given proxy URL.
// If proxyURL is empty, returns the global kiro REST HTTP client.
func GetRestClientForProxy(proxyURL string) *http.Client {
	if proxyURL == "" {
		return kiroRestHttpStore.Load()
	}
	cacheKey := "rest:" + proxyURL
	if cached, ok := proxyClientCache.Load(cacheKey); ok {
		return cached.(*http.Client)
	}
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: buildKiroTransport(proxyURL),
	}
	proxyClientCache.Store(cacheKey, client)
	return client
}

// ResolveAccountProxyURL returns the effective proxy URL for an account.
// Falls back to global config.GetProxyURL() if the account has no per-account proxy.
func ResolveAccountProxyURL(account *config.Account) string {
	if account != nil && account.ProxyURL != "" {
		return account.ProxyURL
	}
	return config.GetProxyURL()
}

// maybeUseTerminator rewrites an HTTPS URL to HTTP when a TLS terminator proxy
// is configured. The terminator handles TLS (Chrome BoringSSL) so the Go proxy
// sends plain HTTP. Used for Kiro API calls only.
func maybeUseTerminator(rawURL string, account *config.Account) string {
	proxyURL := ResolveAccountProxyURL(account)
	if proxyURL == "" {
		return rawURL
	}
	if strings.HasPrefix(rawURL, "https://") {
		return "http://" + strings.TrimPrefix(rawURL, "https://")
	}
	return rawURL
}

// buildKiroTransport constructs an HTTP Transport with optional outbound proxy support.
//
// The dial / TLS-handshake / response-header timeouts bound connection setup and
// time-to-first-byte WITHOUT capping the total response time — an SSE stream can
// still run for as long as the upstream keeps sending, because ResponseHeaderTimeout
// only limits how long we wait for the response headers, not the body. This is the
// streaming-safe alternative to a blanket http.Client.Timeout.
func buildKiroTransport(proxyURL string) *http.Transport {
	logger.Debugf("[TLS] buildKiroTransport proxyURL=%q", proxyURL)
	t := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 300 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    false,
		ForceAttemptHTTP2:     true,
	}
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			// Only route Kiro/AWS API hosts through the terminator.
			// Microsoft login, token refresh, etc. go direct with uTLS.
			t.Proxy = func(req *http.Request) (*url.URL, error) {
				host := req.URL.Hostname()
				if strings.HasSuffix(host, ".amazonaws.com") || strings.HasSuffix(host, ".amazonaws.com.cn") {
					return u, nil
				}
				return nil, nil // direct connection
			}
			t.ForceAttemptHTTP2 = false
		}
	} else {
		t.Proxy = http.ProxyFromEnvironment
	}
	return t
}

// kiroTransportWrapper, when non-nil, wraps the RoundTripper of every Kiro HTTP
// client built by InitKiroHttpClient. It is nil in normal builds (set only by
// the kirotest build tag), so a shipped binary gets the real transport verbatim
// and this adds zero behavior. It exists because InitKiroHttpClient is also
// called at runtime (NewHandler → applyProxyConfig), which would otherwise
// clobber any client a test had swapped in during init.
var kiroTransportWrapper func(http.RoundTripper) http.RoundTripper

// InitKiroHttpClient initializes (or reinitializes) the HTTP clients used for Kiro API requests.
func InitKiroHttpClient(proxyURL string) {
	var streamTransport http.RoundTripper = buildKiroTransport(proxyURL)
	var restTransport http.RoundTripper = buildKiroTransport(proxyURL)
	if kiroTransportWrapper != nil {
		streamTransport = kiroTransportWrapper(streamTransport)
		restTransport = kiroTransportWrapper(restTransport)
	}

	kiroHttpStore.Store(&http.Client{
		Timeout:   5 * time.Minute,
		Transport: streamTransport,
	})
	kiroRestHttpStore.Store(&http.Client{
		Timeout:   30 * time.Second,
		Transport: restTransport,
	})
}

// ==================== Request Structs ====================

// KiroPayload is the top-level request body sent to the Kiro API.
type KiroPayload struct {
	ConversationState struct {
		AgentContinuationId string `json:"agentContinuationId,omitempty"`
		AgentTaskType       string `json:"agentTaskType,omitempty"`
		ChatTriggerType     string `json:"chatTriggerType"`
		ConversationID      string `json:"conversationId"`
		CurrentMessage      struct {
			UserInputMessage KiroUserInputMessage `json:"userInputMessage"`
		} `json:"currentMessage"`
		History []KiroHistoryMessage `json:"history,omitempty"`
	} `json:"conversationState"`
	ProfileArn      string           `json:"profileArn,omitempty"`
	InferenceConfig *InferenceConfig `json:"inferenceConfig,omitempty"`

	// ToolNameMap maps sanitized tool names (sent to Kiro) back to the
	// original names supplied by the client. Used to restore original names
	// in tool_use responses so the client can match them to its tool registry.
	// Not serialized to the Kiro API request body.
	ToolNameMap map[string]string `json:"-"`
}

type KiroUserInputMessage struct {
	Content                 string                   `json:"content"`
	ModelID                 string                   `json:"modelId,omitempty"`
	Origin                  string                   `json:"origin"`
	Images                  []KiroImage              `json:"images,omitempty"`
	UserInputMessageContext *UserInputMessageContext `json:"userInputMessageContext,omitempty"`

	// CachePoint requests an upstream prompt-cache breakpoint after this message.
	// Wire shape mirrors the official codewhisperer-streaming smithy model:
	// userInputMessage.cachePoint = {"type":"default"}. Omitted when nil so the
	// default request shape is unchanged.
	CachePoint *KiroCachePoint `json:"cachePoint,omitempty"`
	// ClientCacheConfig carries the upstream client-cache toggle (clientCacheConfig
	// in the smithy model). Omitted when nil.
	ClientCacheConfig *KiroClientCacheConfig `json:"clientCacheConfig,omitempty"`
}

// KiroCachePoint is the upstream cache breakpoint marker. Type is "default".
type KiroCachePoint struct {
	Type string `json:"type"`
}

// KiroClientCacheConfig mirrors the smithy ClientCacheConfig shape.
type KiroClientCacheConfig struct {
	UseClientCachingOnly bool `json:"useClientCachingOnly,omitempty"`
}

type UserInputMessageContext struct {
	Tools       []KiroToolWrapper `json:"tools,omitempty"`
	ToolResults []KiroToolResult  `json:"toolResults,omitempty"`
}

type KiroToolWrapper struct {
	ToolSpecification struct {
		Name        string      `json:"name"`
		Description string      `json:"description"`
		InputSchema InputSchema `json:"inputSchema"`
	} `json:"toolSpecification"`
}

type InputSchema struct {
	JSON interface{} `json:"json"`
}

type KiroToolResult struct {
	ToolUseID string              `json:"toolUseId"`
	Content   []KiroResultContent `json:"content"`
	Status    string              `json:"status"`
}

type KiroResultContent struct {
	Text string `json:"text"`
}

type KiroImage struct {
	Format string `json:"format"`
	Source struct {
		Bytes string `json:"bytes"`
	} `json:"source"`
}

type KiroHistoryMessage struct {
	UserInputMessage         *KiroUserInputMessage         `json:"userInputMessage,omitempty"`
	AssistantResponseMessage *KiroAssistantResponseMessage `json:"assistantResponseMessage,omitempty"`
}

type KiroAssistantResponseMessage struct {
	Content  string        `json:"content"`
	ToolUses []KiroToolUse `json:"toolUses,omitempty"`
}

type KiroToolUse struct {
	ToolUseID string                 `json:"toolUseId"`
	Name      string                 `json:"name"`
	Input     map[string]interface{} `json:"input"`
}

type InferenceConfig struct {
	MaxTokens   int     `json:"maxTokens,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
	TopP        float64 `json:"topP,omitempty"`
}

// ==================== Stream Callbacks ====================

// KiroStreamCallback stream response callbacks
type KiroStreamCallback struct {
	OnText         func(text string, isThinking bool)
	OnToolUse      func(toolUse KiroToolUse)
	OnComplete     func(inputTokens, outputTokens int)
	OnError        func(err error)
	OnCredits      func(credits float64)
	OnContextUsage func(percentage float64)
	OnStopReason   func(reason string)
}

// ==================== API Call ====================

func setPayloadProfileArnForAccount(payload *KiroPayload, account *config.Account) {
	if payload == nil {
		return
	}

	// API Key credentials must not carry IDE/profile semantics.
	if config.IsAPIKeyAccount(account) {
		payload.ProfileArn = ""
		return
	}

	payload.ProfileArn = strings.TrimSpace(payload.ProfileArn)
	if account != nil {
		if profileArn := strings.TrimSpace(account.ProfileArn); profileArn != "" {
			payload.ProfileArn = profileArn
		}
	}
}

// endpointsForAccount returns the upstream endpoint list for a credential.
// API Key accounts always use the CLI runtime protocol; OAuth accounts keep
// the configured preferred-endpoint fallback chain.
func endpointsForAccount(account *config.Account) []kiroEndpoint {
	if config.IsAPIKeyAccount(account) {
		return []kiroEndpoint{kiroCLIEndpoint}
	}
	return getSortedEndpoints(config.GetPreferredEndpoint())
}

// cliRuntimeURL builds the regional Kiro CLI runtime URL.
func cliRuntimeURL(account *config.Account) string {
	region := "us-east-1"
	if account != nil {
		if r := strings.TrimSpace(account.Region); r != "" {
			region = r
		}
	}
	return fmt.Sprintf("https://runtime.%s.kiro.dev/", region)
}

// getSortedEndpoints returns endpoints ordered by user preference, with optional fallback.
func getSortedEndpoints(preferred string) []kiroEndpoint {
	fallback := config.GetEndpointFallback()

	var primary int
	switch preferred {
	case "kiro":
		primary = 0
	case "codewhisperer":
		primary = 1
	case "amazonq":
		primary = 2
	default:
		// "auto": Kiro first, then fallback to others
		return []kiroEndpoint{kiroEndpoints[0], kiroEndpoints[1], kiroEndpoints[2]}
	}

	if !fallback {
		// No fallback: only use the selected endpoint
		return []kiroEndpoint{kiroEndpoints[primary]}
	}

	// With fallback: selected first, then others in order
	result := []kiroEndpoint{kiroEndpoints[primary]}
	for i, ep := range kiroEndpoints {
		if i != primary {
			result = append(result, ep)
		}
	}
	return result
}

// secretPreviewRe masks obvious credential tokens in the content preview so
// debug logs never leak API keys / bearer tokens that appear inside prompts.
var secretPreviewRe = regexp.MustCompile(`(?i)(sk-[a-z0-9_-]{6,}|bearer\s+[a-z0-9._-]{8,}|(?:api[_-]?key|token|secret|password)["']?\s*[:=]\s*["']?[a-z0-9._-]{6,})`)

func maskSecrets(s string) string {
	return secretPreviewRe.ReplaceAllString(s, "[REDACTED]")
}

// summarizeKiroPayload returns a compact, single-line description of a request
// payload for debug logging: the request shape (model, history depth, tool
// counts, content size) plus a short, secret-masked preview of the current
// message content. It deliberately avoids dumping the full payload, which can
// be hundreds of KB and contain user secrets.
func summarizeKiroPayload(payload *KiroPayload) string {
	if payload == nil {
		return "<nil>"
	}
	cs := &payload.ConversationState
	uim := &cs.CurrentMessage.UserInputMessage

	tools, toolResults := 0, 0
	if uim.UserInputMessageContext != nil {
		tools = len(uim.UserInputMessageContext.Tools)
		toolResults = len(uim.UserInputMessageContext.ToolResults)
	}

	const previewLen = 200
	preview := uim.Content
	truncated := false
	if len([]rune(preview)) > previewLen {
		preview = string([]rune(preview)[:previewLen])
		truncated = true
	}
	// Collapse whitespace/newlines so the preview stays on one log line.
	preview = strings.Join(strings.Fields(preview), " ")
	preview = maskSecrets(preview)
	if truncated {
		preview += "…"
	}

	convID := cs.ConversationID
	if len(convID) > 8 {
		convID = convID[:8]
	}

	return fmt.Sprintf("conv=%s model=%s task=%s trigger=%s history=%d tools=%d toolResults=%d images=%d contentChars=%d content=%q",
		convID, uim.ModelID, cs.AgentTaskType, cs.ChatTriggerType,
		len(cs.History), tools, toolResults, len(uim.Images), len(uim.Content), preview)
}

// CallKiroAPI is the context-free entry point kept for existing callers and
// tests. It delegates to CallKiroAPIContext with a background context.
func CallKiroAPI(account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	return CallKiroAPIContext(context.Background(), account, payload, callback)
}

// CallKiroAPIContext calls Kiro while respecting cancellation from the client request.
func CallKiroAPIContext(ctx context.Context, account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	if ctx == nil {
		ctx = context.Background()
	}
	originalProfileArn := ""
	if payload != nil {
		originalProfileArn = payload.ProfileArn
		defer func() {
			payload.ProfileArn = originalProfileArn
		}()
	}
	setPayloadProfileArnForAccount(payload, account)

	if _, err := json.Marshal(payload); err != nil {
		return err
	}

	// Debug: dump full payload for troubleshooting upstream rejections
	if payloadJSON, err := json.Marshal(payload); err == nil {
		logger.Debugf("[KiroAPI] Request payload: %s", string(payloadJSON))
	}

	// Wrap OnToolUse to restore original tool names for the client.
	if callback != nil && callback.OnToolUse != nil && len(payload.ToolNameMap) > 0 {
		originalOnToolUse := callback.OnToolUse
		nameMap := payload.ToolNameMap
		wrapped := *callback
		wrapped.OnToolUse = func(tu KiroToolUse) {
			if original, ok := nameMap[tu.Name]; ok {
				tu.Name = original
			}
			originalOnToolUse(tu)
		}
		callback = &wrapped
	}

	if payload != nil && strings.TrimSpace(payload.ProfileArn) == "" && !config.IsAPIKeyAccount(account) {
		if profileArn, err := ResolveProfileArn(account); err == nil {
			payload.ProfileArn = profileArn
		} else if isProfileArnResolutionSoftError(err) {
			logger.Debugf("[ProfileArn] Skipped profile ARN resolution for %s: %v", accountEmailForLog(account), err)
		} else {
			logger.Warnf("[ProfileArn] Failed to resolve profile ARN for %s: %v", accountEmailForLog(account), err)
		}
	}

	// Build endpoint list ordered by configuration / credential type.
	endpoints := resolveKiroEndpoints(account)
	isAPIKey := config.IsAPIKeyAccount(account)

	// Experimental: route to the current-generation Kiro runtime backend
	// (runtime.{region}.kiro.dev, path "/", the host the current Kiro CLI uses)
	// instead of the legacy q/codewhisperer hosts. Gated by env, off by default.
	// Unlike the legacy hosts this backend emits metadataEvent + initial-response
	// frames; it does NOT emit token-level cache usage, but it does cache repeated
	// prefixes automatically (visible only as a meteringEvent credit drop). No
	// fallback on purpose: a runtime failure must surface, not silently divert to
	// codewhisperer. See docs/kiro-prompt-cache-verification-2026-07-29.md.
	if os.Getenv("KIRO_RUNTIME_ENDPOINT") == "1" {
		region := kiroRegion(account)
		if region == "" {
			region = "us-east-1"
		}
		endpoints = []kiroEndpoint{{
			URL:       fmt.Sprintf("https://runtime.%s.kiro.dev/", region),
			Origin:    "AI_EDITOR",
			AmzTarget: "AmazonCodeWhispererStreamingService.GenerateAssistantResponse",
			Name:      "KiroRuntime",
		}}
	}

	// Inter-request jitter: add 200-800ms random delay before first attempt
	// to avoid machine-like timing patterns that trigger anti-abuse detection.
	jitterBase := 200
	if len(endpoints) > 1 {
		// More endpoints = more attempts = more jitter needed
		jitterBase = 400
	}
	time.Sleep(time.Duration(jitterBase+randIntn(600)) * time.Millisecond)

	var lastErr error
endpointLoop:
	for epIndex, ep := range endpoints {
		// Update the origin field for the selected endpoint.
		payload.ConversationState.CurrentMessage.UserInputMessage.Origin = ep.Origin

		// Target the profile's data-plane region; endpoint URLs are declared for us-east-1.
		// API Key accounts use the CLI runtime host instead of IDE/Q hosts.
		epURL := regionalizeURLForProfile(ep.URL, account, payload.ProfileArn)
		if err := ctx.Err(); err != nil {
			return err
		}
		if isAPIKey {
			epURL = cliRuntimeURL(account)
		}

		reqBody, _ := json.Marshal(payload)
		host := ""
		if parsedURL, parseErr := url.Parse(epURL); parseErr == nil {
			host = parsedURL.Host
		}
		// The runtime (kiro.dev) host emulates the current Kiro CLI, which pins
		// codewhispererruntime headers; the legacy hosts use codewhispererstreaming.
		headerValues := buildStreamingHeaderValues(account, host)
		if ep.Name == "KiroRuntime" {
			headerValues = buildRuntimeHeaderValues(account, host)
		}
		invocationID := uuid.New().String()

		for streamAttempt := 1; streamAttempt <= maxStreamAttemptsPerEndpoint; streamAttempt++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			// Requests and bodies cannot be reused after an HTTP attempt.
			req, err := http.NewRequestWithContext(ctx, "POST", epURL, bytes.NewReader(reqBody))
			if err != nil {
				lastErr = err
				continue endpointLoop
			}
			// runtime.kiro.dev is an AWS Coral service that dispatches the operation by
			// the Content-Type + X-Amz-Target pair. Plain application/json yields
			// UnknownOperationException; the current Kiro CLI sends x-amz-json-1.0.
			// API-key accounts reach that same runtime via cliRuntimeURL, so both
			// arms of this condition target Coral hosts.
			if isAPIKey || ep.Name == "KiroRuntime" {
				req.Header.Set("Content-Type", "application/x-amz-json-1.0")
			} else {
				req.Header.Set("Content-Type", "application/json")
			}
			req.Header.Set("Accept", "*/*")
			if ep.AmzTarget != "" {
				req.Header.Set("X-Amz-Target", ep.AmzTarget)
			}
			applyKiroBaseHeaders(req, account, headerValues)
			if !isAPIKey {
				req.Header.Set("x-amzn-kiro-agent-mode", "vibe")
			}
			// CLI captures use optout=false; IDE path keeps true.
			if isAPIKey {
				req.Header.Set("x-amzn-codewhisperer-optout", "false")
			} else {
				req.Header.Set("x-amzn-codewhisperer-optout", "true")
			}
			req.Header.Set("Amz-Sdk-Request", fmt.Sprintf("attempt=%d; max=%d", streamAttempt, maxStreamAttemptsPerEndpoint))
			req.Header.Set("Amz-Sdk-Invocation-Id", invocationID)

			resp, err := GetClientForProxy(ResolveAccountProxyURL(account)).Do(req)
			if err != nil {
				lastErr = err
				logger.Warnf("[KiroAPI] Endpoint %s failed: %v", ep.Name, err)
				if !isRetryableStreamError(err) {
					return err
				}
				continue endpointLoop
			}

			if resp.StatusCode != 200 {
				// The body must be read even on 429. AWS reports two different
				// failures with that one status: real quota exhaustion, and an
				// anti-abuse throttle whose body says "suspicious activity". They
				// need opposite handling — a fixed quota cooldown versus exponential
				// backoff, because each rapid retry renews AWS's investigation timer
				// — and the body is the only thing that distinguishes them. Closing
				// it unread (which this branch used to do for 429) made
				// KiroErrAntiAbuse unreachable in production and every throttle look
				// like an exhausted account.
				errBody, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				body := string(errBody)
				upstreamErr := &KiroUpstreamError{
					Category:   categorizeUpstream(resp.StatusCode, body),
					StatusCode: resp.StatusCode,
					Endpoint:   ep.Name,
					Body:       body,
				}
				lastErr = upstreamErr

				switch upstreamErr.Category {
				case KiroErrAntiAbuse:
					logger.Warnf("[KiroAPI] Endpoint %s anti-abuse throttle (429), trying next...", ep.Name)
				case KiroErrQuota:
					logger.Warnf("[KiroAPI] Endpoint %s quota exhausted (429), trying next...", ep.Name)
				default:
					logger.Warnf("[KiroAPI] Endpoint %s error: %v", ep.Name, lastErr)
				}

				// Authentication errors and payment errors are not retried across endpoints.
				if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 402 {
					return lastErr
				}
				continue endpointLoop
			}

			emitted, err := parseEventStreamTracked(resp.Body, callback)
			resp.Body.Close()
			if err == nil {
				return nil
			}
			lastErr = err
			// "Emitted" deliberately means that an output callback ran. This
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			// conservative boundary also protects buffered/non-stream callers:
			// retrying after their callback mutated state would concatenate two
			// attempts even if no network bytes were flushed yet.
			if emitted {
				return err
			}
			if !isRetryableStreamError(err) {
				return err
			}

			hasSameEndpointRetry := streamAttempt < maxStreamAttemptsPerEndpoint
			hasEndpointFallback := epIndex+1 < len(endpoints)
			if !hasSameEndpointRetry && !hasEndpointFallback {
				break endpointLoop
			}

			logger.Warnf("[KiroAPI] Endpoint %s stream failed before any output (attempt %d/%d): %v",
				ep.Name, streamAttempt, maxStreamAttemptsPerEndpoint, err)
			if err := streamRetryWait(ctx, streamRetryBackoff); err != nil {
				return err
			}
			if !hasSameEndpointRetry {
				continue endpointLoop
			}
		}
	}

	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("all endpoints failed")
}

func isRetryableStreamError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var netErr net.Error
	return !errors.As(err, &netErr) || !netErr.Timeout()
}

func accountEmailForLog(account *config.Account) string {
	if account == nil {
		return "<nil>"
	}
	return account.Email
}

func waitForStreamRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// ==================== Event Stream Parsing ====================

// parseEventStream decodes an AWS binary Event Stream response body.
func parseEventStream(body io.Reader, callback *KiroStreamCallback) error {
	_, err := parseEventStreamTracked(body, callback)
	return err
}

// parseEventStreamTracked is parseEventStream plus a report of whether an
// output callback was invoked. A failure before the first callback is safe to
// retry because it cannot duplicate or concatenate caller state.
func parseEventStreamTracked(body io.Reader, callback *KiroStreamCallback) (emitted bool, err error) {
	if callback == nil {
		callback = &KiroStreamCallback{}
	}

	// Read directly without bufio to avoid buffering latency in streaming responses.
	var inputTokens, outputTokens int
	var totalCredits float64
	var contextUsagePercentages []float64
	var sawOutput bool
	pending := &pendingToolUses{}
	trackedCallback := *callback
	originalOnToolUse := trackedCallback.OnToolUse
	trackedCallback.OnToolUse = func(toolUse KiroToolUse) {
		sawOutput = true
		if originalOnToolUse != nil {
			emitted = true
			originalOnToolUse(toolUse)
		}
	}
	callback = &trackedCallback

	for {
		// Prelude: total length, headers length, and prelude CRC.
		prelude := make([]byte, 12)
		_, readErr := io.ReadFull(body, prelude)
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return emitted, readErr
		}

		totalLength := int(eventStreamUint32(prelude[0:4]))
		headersLength := int(eventStreamUint32(prelude[4:8]))
		if totalLength < 16 || totalLength > maxEventStreamMessageSize {
			return emitted, fmt.Errorf("%w: invalid frame length %d", errInvalidKiroEventStream, totalLength)
		}
		if headersLength > totalLength-16 {
			return emitted, fmt.Errorf("%w: invalid headers length %d", errInvalidKiroEventStream, headersLength)
		}
		if got, want := eventStreamUint32(prelude[8:12]), crc32.ChecksumIEEE(prelude[:8]); got != want {
			return emitted, fmt.Errorf("%w: prelude CRC mismatch", errInvalidKiroEventStream)
		}

		remaining := totalLength - len(prelude)
		message := make([]byte, remaining)
		if _, err := io.ReadFull(body, message); err != nil {
			return emitted, err
		}
		if got, want := eventStreamUint32(message[len(message)-4:]), eventStreamChecksum(prelude, message[:len(message)-4]); got != want {
			return emitted, fmt.Errorf("%w: message CRC mismatch", errInvalidKiroEventStream)
		}

		headers, headerErr := parseEventStreamHeaders(message[:headersLength])
		if headerErr != nil {
			return emitted, fmt.Errorf("%w: %v", errInvalidKiroEventStream, headerErr)
		}
		payloadBytes := message[headersLength : len(message)-4]
		event := make(map[string]interface{})
		if len(payloadBytes) > 0 {
			if err := json.Unmarshal(payloadBytes, &event); err != nil {
				return emitted, fmt.Errorf("%w: decode payload: %v", errInvalidKiroEventStream, err)
			}
		}

		if messageType := headers[":message-type"]; messageType == "error" || messageType == "exception" {
			detail, _ := event["message"].(string)
			if detail == "" {
				detail = messageType
			}
			return emitted, fmt.Errorf("%w: %s", errKiroEventStreamUpstream, detail)
		}

		inputTokens, outputTokens = updateTokensFromEvent(event, inputTokens, outputTokens)

		switch headers[":event-type"] {
		case "assistantResponseEvent":
			if content, ok := event["content"].(string); ok && content != "" {
				sawOutput = true
				if callback.OnText != nil {
					emitted = true
					callback.OnText(content, false)
				}
			}
		case "reasoningContentEvent":
			if text, ok := event["text"].(string); ok && text != "" {
				sawOutput = true
				if callback.OnText != nil {
					emitted = true
					callback.OnText(text, true)
				}
			}
		case "toolUseEvent":
			if toolErr := handleToolUseEvent(event, pending, callback); toolErr != nil {
				return emitted, toolErr
			}
		case "meteringEvent":
			if usage, ok := event["usage"].(float64); ok {
				totalCredits += usage
			}
		case "contextUsageEvent":
			if pct, ok := event["contextUsagePercentage"].(float64); ok {
				contextUsagePercentages = append(contextUsagePercentages, pct)
			}
		case "metadataEvent":
			// stopReason rides inside metadataEvent on the wire; there is no
			// standalone stop reason event type. Its absence after content is
			// how callers detect a truncated stream.
			if reason := firstStringField(event, "stopReason", "stop_reason"); reason != "" && callback.OnStopReason != nil {
				callback.OnStopReason(reason)
			}
		}
	}

	// Flush tools that never received a stop frame, in arrival order.
	if err := pending.flushAll(callback); err != nil {
		return emitted, err
	}
	if !sawOutput {
		return emitted, errEmptyKiroStream
	}
	if callback.OnCredits != nil && totalCredits > 0 {
		callback.OnCredits(totalCredits)
	}
	if callback.OnContextUsage != nil {
		for _, percentage := range contextUsagePercentages {
			callback.OnContextUsage(percentage)
		}
	}
	if callback.OnComplete != nil {
		callback.OnComplete(inputTokens, outputTokens)
	}
	return emitted, nil
}

func updateTokensFromEvent(event map[string]interface{}, currentInputTokens, currentOutputTokens int) (int, int) {
	candidates := []map[string]interface{}{event}
	collectUsageMaps(event, &candidates)

	inputTokens := currentInputTokens
	outputTokens := currentOutputTokens

	for _, usage := range candidates {
		if usage == nil {
			continue
		}

		if v, ok := readTokenNumber(usage,
			"outputTokens", "completionTokens", "totalOutputTokens",
			"output_tokens", "completion_tokens", "total_output_tokens",
		); ok {
			outputTokens = v
		}

		if v, ok := readTokenNumber(usage,
			"inputTokens", "promptTokens", "totalInputTokens",
			"input_tokens", "prompt_tokens", "total_input_tokens",
		); ok {
			inputTokens = v
			continue
		}

		uncached, _ := readTokenNumber(usage, "uncachedInputTokens", "uncached_input_tokens")
		cacheRead, _ := readTokenNumber(usage, "cacheReadInputTokens", "cache_read_input_tokens")
		cacheWrite, _ := readTokenNumber(usage, "cacheWriteInputTokens", "cache_write_input_tokens", "cacheCreationInputTokens", "cache_creation_input_tokens")
		if uncached+cacheRead+cacheWrite > 0 {
			inputTokens = uncached + cacheRead + cacheWrite
			continue
		}

		total, ok := readTokenNumber(usage, "totalTokens", "total_tokens")
		if ok && total > 0 {
			candidateOutput := outputTokens
			if v, vok := readTokenNumber(usage,
				"outputTokens", "completionTokens", "totalOutputTokens",
				"output_tokens", "completion_tokens", "total_output_tokens",
			); vok {
				candidateOutput = v
			}
			if total-candidateOutput > 0 {
				inputTokens = total - candidateOutput
			}
		}
	}

	return inputTokens, outputTokens
}

// getContextWindowSize returns the context window size (in tokens) for a model.
//
// Per Kiro's ListAvailableModels, the 1M-token context window applies to
// Claude 4.6 and newer (sonnet-4.6, opus-4.6, opus-4.7, opus-4.8, and future
// 4.x releases), while 4.5 and earlier (opus-4.5, sonnet-4.5, sonnet-4,
// haiku-4.5) use a 200K window. This value is used to convert the upstream
// contextUsagePercentage into an absolute input-token count that clients rely
// on to decide when to compact; an undersized window under-reports tokens and
// prevents clients from compacting in time.
func getContextWindowSize(model string) int {
	if isLargeContextModel(model) {
		return 1_000_000
	}
	return 200_000
}

// claudeVersionExtractor matches "claude-<family>-<major>[.<minor>]" (dot or
// dash form) and is used to classify 1M-window models by version. The minor
// component is optional so major-only identifiers such as "claude-opus-5"
// classify correctly instead of falling through to the 200K default.
var claudeVersionExtractor = regexp.MustCompile(`claude-(?:opus|sonnet|haiku)-(\d+)(?:[.-](\d+))?`)

func isLargeContextModel(model string) bool {
	m := strings.ToLower(model)
	if match := claudeVersionExtractor.FindStringSubmatch(m); match != nil {
		major, errMaj := strconv.Atoi(match[1])
		if errMaj == nil {
			// 1M window for any major >= 5 (claude-opus-5, claude-opus-5.1, ...).
			if major > 4 {
				return true
			}
			// Within Claude 4.x the window depends on the minor version, so an
			// absent minor (claude-sonnet-4) is treated as 4.0 -> 200K.
			minor := 0
			if match[2] != "" {
				parsed, errMin := strconv.Atoi(match[2])
				if errMin != nil {
					return false
				}
				minor = parsed
			}
			return major == 4 && minor >= 6
		}
	}
	// Fallback substring checks for non-standard identifiers.
	for _, tag := range []string{"4.6", "4-6", "4.7", "4-7", "4.8", "4-8", "4.9", "4-9"} {
		if strings.Contains(m, tag) {
			return true
		}
	}
	return false
}

func collectUsageMaps(v interface{}, out *[]map[string]interface{}) {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, child := range t {
			lk := strings.ToLower(k)
			if lk == "usage" || lk == "tokenusage" || lk == "token_usage" {
				if m, ok := child.(map[string]interface{}); ok {
					*out = append(*out, m)
				}
			}
			collectUsageMaps(child, out)
		}
	case []interface{}:
		for _, child := range t {
			collectUsageMaps(child, out)
		}
	}
}

func readTokenNumber(m map[string]interface{}, keys ...string) (int, bool) {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch n := v.(type) {
		case float64:
			return int(n), true
		case int:
			return n, true
		case int64:
			return int(n), true
		case json.Number:
			if parsed, err := n.Int64(); err == nil {
				return int(parsed), true
			}
		case string:
			if parsed, err := strconv.Atoi(n); err == nil {
				return parsed, true
			}
			if parsed, err := strconv.ParseFloat(n, 64); err == nil {
				return int(parsed), true
			}
		}
	}
	return 0, false
}

// ==================== Tool Use Handling ====================

type toolUseState struct {
	ToolUseID   string
	Name        string
	InputBuffer strings.Builder
	GeneratedID bool
}

// pendingToolUses tracks the tool calls in flight for one stream. Entries are
// keyed by toolUseId so interleaved parallel frames accumulate independently,
// and `order` preserves arrival sequence: tool call order is semantic for
// clients, so a bare map range (randomised in Go) must never decide it.
type pendingToolUses struct {
	byID   map[string]*toolUseState
	order  []string
	lastID string
}

func (p *pendingToolUses) get(id string) *toolUseState {
	if p.byID == nil {
		return nil
	}
	return p.byID[id]
}

func (p *pendingToolUses) add(state *toolUseState) {
	if p.byID == nil {
		p.byID = make(map[string]*toolUseState)
	}
	p.byID[state.ToolUseID] = state
	p.order = append(p.order, state.ToolUseID)
	p.lastID = state.ToolUseID
}

// rekey moves an entry from a locally generated id to the real id from
// upstream, keeping its position in the arrival order.
func (p *pendingToolUses) rekey(state *toolUseState, newID string) {
	oldID := state.ToolUseID
	delete(p.byID, oldID)
	for i, id := range p.order {
		if id == oldID {
			p.order[i] = newID
			break
		}
	}
	state.ToolUseID = newID
	state.GeneratedID = false
	p.byID[newID] = state
	if p.lastID == oldID {
		p.lastID = newID
	}
}

func (p *pendingToolUses) remove(id string) {
	delete(p.byID, id)
	for i, existing := range p.order {
		if existing == id {
			p.order = append(p.order[:i], p.order[i+1:]...)
			break
		}
	}
	if p.lastID == id {
		p.lastID = ""
	}
}

// flushAll emits every tool still open when the stream ended, in arrival order.
// It stops at the first incomplete tool so the caller can retry the request
// rather than deliver a call with missing arguments.
func (p *pendingToolUses) flushAll(callback *KiroStreamCallback) error {
	order := p.order
	byID := p.byID
	p.byID = nil
	p.order = nil
	p.lastID = ""
	for _, id := range order {
		state := byID[id]
		if state == nil {
			continue
		}
		if err := finishToolUse(state, callback); err != nil {
			return err
		}
	}
	return nil
}

// handleToolUseEvent accumulates one toolUseEvent frame into pending.
// Frames for different toolUseIds stay open concurrently so interleaved
// parallel tool calls reassemble correctly; a fragment that omits toolUseId
// continues the most recently seen tool.
func handleToolUseEvent(event map[string]interface{}, pending *pendingToolUses, callback *KiroStreamCallback) error {
	toolUseID := firstStringField(event, "toolUseId", "toolUseID", "tool_use_id", "id")
	name := firstStringField(event, "name", "toolName", "tool_name")
	isStop := firstBoolField(event, "stop", "isStop", "done")

	var state *toolUseState

	switch {
	case toolUseID != "":
		state = pending.get(toolUseID)
		if state == nil && pending.lastID != "" {
			// Upstream may send the opening fragment without an id and only
			// reveal the real one later. Adopt the synthetic entry we opened
			// for it instead of splitting one logical call across two entries
			// (which also splits its argument JSON, so neither half parses).
			if prev := pending.get(pending.lastID); prev != nil && prev.GeneratedID && (name == "" || prev.Name == name) {
				pending.rekey(prev, toolUseID)
				state = prev
			}
		}
		if state == nil {
			if name == "" {
				// Orphan fragment: an id never seen before and no name to open with.
				return nil
			}
			state = &toolUseState{ToolUseID: toolUseID, Name: name}
			pending.add(state)
		} else {
			if name != "" && state.Name == "" {
				state.Name = name
			}
			pending.lastID = state.ToolUseID
		}
	case pending.lastID != "" && pending.get(pending.lastID) != nil:
		state = pending.get(pending.lastID)
		if name != "" && state.Name != name {
			// Name changed with no id: close the tool in flight rather than
			// appending foreign arguments to it, then open a new one.
			if err := finishToolUse(state, callback); err != nil {
				return err
			}
			pending.remove(state.ToolUseID)
			state = &toolUseState{ToolUseID: "toolu_" + uuid.New().String(), Name: name, GeneratedID: true}
			pending.add(state)
		}
	case name != "":
		state = &toolUseState{ToolUseID: "toolu_" + uuid.New().String(), Name: name, GeneratedID: true}
		pending.add(state)
	default:
		return nil
	}

	if input, ok := event["input"].(string); ok {
		state.InputBuffer.WriteString(input)
	} else if inputObj, ok := event["input"].(map[string]interface{}); ok {
		data, _ := json.Marshal(inputObj)
		state.InputBuffer.Reset()
		state.InputBuffer.Write(data)
	}

	if isStop {
		if err := finishToolUse(state, callback); err != nil {
			return err
		}
		pending.remove(state.ToolUseID)
	}
	return nil
}

func finishToolUse(state *toolUseState, callback *KiroStreamCallback) error {
	if state == nil || state.Name == "" {
		return nil
	}
	if state.ToolUseID == "" {
		state.ToolUseID = "toolu_" + uuid.New().String()
	}
	var input map[string]interface{}
	if state.InputBuffer.Len() > 0 {
		if err := json.Unmarshal([]byte(state.InputBuffer.String()), &input); err != nil {
			return fmt.Errorf("%w: %v", errIncompleteKiroToolInput, err)
		}
	}
	if input == nil {
		input = make(map[string]interface{})
	}
	if callback == nil || callback.OnToolUse == nil {
		return nil
	}
	callback.OnToolUse(KiroToolUse{
		ToolUseID: state.ToolUseID,
		Name:      state.Name,
		Input:     input,
	})
	return nil
}

func firstStringField(m map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func firstBoolField(m map[string]interface{}, keys ...string) bool {
	for _, key := range keys {
		if v, ok := m[key].(bool); ok {
			return v
		}
	}
	return false
}

func eventStreamUint32(data []byte) uint32 {
	return uint32(data[0])<<24 | uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3])
}

func eventStreamChecksum(prelude, message []byte) uint32 {
	checksum := crc32.Update(0, crc32.IEEETable, prelude)
	return crc32.Update(checksum, crc32.IEEETable, message)
}

func parseEventStreamHeaders(data []byte) (map[string]string, error) {
	headers := make(map[string]string)
	for offset := 0; offset < len(data); {
		nameLength := int(data[offset])
		offset++
		if nameLength == 0 || offset+nameLength >= len(data) {
			return nil, errors.New("malformed event header name")
		}
		name := string(data[offset : offset+nameLength])
		offset += nameLength
		valueType := data[offset]
		offset++

		valueLength := 0
		switch valueType {
		case 0, 1:
			continue
		case 2:
			valueLength = 1
		case 3:
			valueLength = 2
		case 4:
			valueLength = 4
		case 5, 8:
			valueLength = 8
		case 9:
			valueLength = 16
		case 6, 7:
			if offset+2 > len(data) {
				return nil, errors.New("malformed variable event header")
			}
			valueLength = int(data[offset])<<8 | int(data[offset+1])
			offset += 2
		default:
			return nil, fmt.Errorf("unsupported event header type %d", valueType)
		}
		if offset+valueLength > len(data) {
			return nil, errors.New("truncated event header value")
		}
		if valueType == 7 {
			headers[name] = string(data[offset : offset+valueLength])
		}
		offset += valueLength
	}
	return headers, nil
}

// randIntn returns a random integer in [0, n).
func randIntn(n int) int {
	return rand.Intn(n)
}
