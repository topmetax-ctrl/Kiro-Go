package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"kiro-go/apikey"
	"kiro-go/logger"
	"kiro-go/providererr"
	"kiro-go/search"
	"net/http"
	"strings"
)

func classifyGoError(err error, requestID, source, endpoint, model, accountID, providerID, providerName string) providererr.InternalError {
	var in providererr.InternalError
	var ke *KiroUpstreamError
	var pe *search.ProviderError
	switch {
	case errors.As(err, &ke):
		in = providererr.FromHTTP(ke.StatusCode, []byte(ke.Body), nil)
		in.Source = "kiro"
		in.Endpoint = ke.Endpoint
	case errors.As(err, &pe):
		var body []byte
		if pe.Err != nil {
			body = []byte(pe.Err.Error())
		}
		status := pe.StatusCode
		if status == 0 {
			switch pe.Kind {
			case search.ErrAuth:
				status = 403
			case search.ErrRateLimit:
				status = 429
			case search.ErrTimeout:
				status = 504
			case search.ErrInvalid:
				status = 400
			default:
				status = 502
			}
		}
		in = providererr.FromHTTP(status, body, nil)
		in.Source = "search"
	case errors.Is(err, errUpstreamTruncatedResponse):
		in = providererr.InternalError{
			Category:  providererr.CategoryError,
			Retryable: true,
			Detail:    "truncated response",
		}
		in.Source = source
	case err != nil:
		in = providererr.FromNetwork(err)
		in.Source = source
	default:
		in.Category = providererr.CategoryInternal
		in.Source = source
	}
	in.RequestID = requestID
	if in.Endpoint == "" {
		in.Endpoint = endpoint
	}
	in.ClientModel = model
	in.EffectiveModel = model
	in.AccountID = accountID
	in.ProviderID = providerID
	in.ProviderName = providerName
	if in.Source == "" {
		in.Source = source
	}
	return in
}

func (h *Handler) persistProviderError(in providererr.InternalError) {
	if h == nil || h.keys == nil || strings.TrimSpace(in.RequestID) == "" {
		return
	}
	pub := in.Public()
	if err := h.keys.PutProviderErrorDetail(apikey.ProviderErrorDetail{
		RequestID:         in.RequestID,
		Attempt:           in.Attempt,
		ProviderID:        in.ProviderID,
		ProviderName:      in.ProviderName,
		ConnectionID:      in.ConnectionID,
		ConnectionName:    in.ConnectionName,
		AccountID:         in.AccountID,
		Endpoint:          in.Endpoint,
		ClientModel:       in.ClientModel,
		EffectiveModel:    in.EffectiveModel,
		UpstreamStatus:    in.UpstreamStatus,
		UpstreamCode:      in.UpstreamCode,
		UpstreamMessage:   in.UpstreamMessage,
		UpstreamRequestID: in.UpstreamRequestID,
		RetryAfter:        in.RetryAfter,
		Category:          string(in.Category),
		PublicCode:        pub.Code,
		Detail:            in.Detail,
		DetailTruncated:   in.DetailTruncated,
		CreatedAt:         in.Timestamp,
	}); err != nil {
		providererr.IncPersistFailed()
		logger.Warnf("[ProviderErr] persist request=%s: %v", in.RequestID, err)
	}
}

func requestIDFromWriter(w http.ResponseWriter) string {
	if ctx := leaseContextFromWriter(w); ctx != nil {
		return requestIDFromContext(ctx)
	}
	return ""
}

func setRequestIDHeader(w http.ResponseWriter, id string) {
	if id != "" && w.Header().Get("X-Request-Id") == "" {
		w.Header().Set("X-Request-Id", id)
	}
}

func (h *Handler) sendPublicClaudeError(w http.ResponseWriter, pub providererr.PublicError) {
	if pub.RequestID == "" {
		pub.RequestID = requestIDFromWriter(w)
	}
	if ctx := leaseContextFromWriter(w); ctx != nil {
		noteAPIKeyPublicError(ctx, pub)
	}
	setRequestIDHeader(w, pub.RequestID)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(pub.HTTPStatus)
	body := map[string]interface{}{
		"type": "error",
		"error": map[string]string{
			"type":    providererr.EnvelopeType(pub.Code, true),
			"message": pub.MessageOrDefault(),
		},
	}
	if pub.RequestID != "" {
		body["request_id"] = pub.RequestID
	}
	_ = json.NewEncoder(w).Encode(body)
}

func (h *Handler) sendPublicOpenAIError(w http.ResponseWriter, pub providererr.PublicError) {
	if pub.RequestID == "" {
		pub.RequestID = requestIDFromWriter(w)
	}
	if ctx := leaseContextFromWriter(w); ctx != nil {
		noteAPIKeyPublicError(ctx, pub)
	}
	setRequestIDHeader(w, pub.RequestID)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(pub.HTTPStatus)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"type":       providererr.EnvelopeType(pub.Code, false),
			"message":    pub.MessageOrDefault(),
			"code":       pub.Code,
			"request_id": pub.RequestID,
		},
	})
}

func (h *Handler) sendPublicForwardError(w http.ResponseWriter, isClaude bool, pub providererr.PublicError) {
	if isClaude {
		h.sendPublicClaudeError(w, pub)
		return
	}
	h.sendPublicOpenAIError(w, pub)
}

func noteAPIKeyPublicError(ctx context.Context, pub providererr.PublicError) {
	if leaseFromContext(ctx) == nil {
		return
	}
	noteAPIKeyOutcome(ctx, apikey.CommitInput{
		Outcome:        outcomeForHTTPStatus(pub.HTTPStatus),
		StatusCode:     pub.HTTPStatus,
		ErrorCode:      pub.Code,
		SanitizedError: pub.MessageOrDefault(),
	})
}

func (h *Handler) recordUpstreamFailure(ctx context.Context, endpoint, model, accountID, providerID, providerName string, err error) providererr.PublicError {
	in := classifyGoError(err, requestIDFromContext(ctx), endpoint, endpoint, model, accountID, providerID, providerName)
	h.persistProviderError(in)
	return in.Public()
}

func publicSSEMessage(err error, requestID string, claude bool) (string, providererr.PublicError) {
	in := classifyGoError(err, requestID, "stream", "", "", "", "", "")
	pub := in.Public()
	pub.RequestID = requestID
	d := providererr.DialectOpenAI
	if claude {
		d = providererr.DialectClaude
	}
	return string(providererr.EncodeSSEError(pub, d)), pub
}

// ssePublicFilter rewrites provider error frames on an already-committed stream.
//
// FRAME-BOUND POLICY. The filter's job is two-sided, and the second side is
// easy to get wrong:
//
//   - Error frames (any dialect's shape, including response.failed) are
//     replaced with the public error so upstream internals never reach the
//     client.
//   - LEGITIMATE giant frames must pass through BYTE-FOR-BYTE. OpenAI
//     Responses' response.completed/response.incomplete carry the entire
//     generated output beside their usage, so long answers make these frames
//     far larger than any sane frame buffer — and they are the only copy the
//     client ever gets. Replacing an over-long frame wholesale (the old
//     behavior) spliced a public error into the middle of a healthy stream and
//     dropped the answer's head. Oversized frames whose head names a content
//     event therefore switch the filter into bypass: bytes stream through
//     verbatim, and only the frame terminator is tracked so normal operation
//     resumes after the frame. Oversized frames that could be errors (or
//     garbage) keep the old replacement — the memory bound holds, and a giant
//     error frame's upstream detail stays suppressed.
type ssePublicFilter struct {
	dst       io.Writer
	flusher   http.Flusher
	dialect   providererr.Dialect
	pub       providererr.PublicError
	onRewrite func(raw string)
	buf       []byte
	// bypass is active while one known-giant content frame streams through
	// verbatim. pendNL holds back a single trailing '\n' so the "\n\n"
	// terminator is still detectable without buffering the frame.
	bypass bool
	pendNL bool
}

func (f *ssePublicFilter) Write(p []byte) (int, error) {
	if f.bypass {
		n, err := f.bypassWrite(p)
		if err != nil {
			return len(p), err
		}
		p = p[n:]
		if len(p) == 0 {
			return len(p), nil
		}
	}
	f.buf = append(f.buf, p...)
	for {
		idx := bytes.Index(f.buf, []byte("\n\n"))
		if idx < 0 {
			if len(f.buf) > providererr.MaxParseBytes {
				if isKnownGiantContentFrame(f.buf) {
					// A content frame the client needs whole: pass the head
					// through verbatim and stop inspecting this frame. Nothing
					// is buffered past this point, so the memory bound holds.
					head := f.buf
					f.buf = f.buf[:0]
					f.bypass = true
					f.pendNL = false
					if _, err := f.dst.Write(head); err != nil {
						return len(p), err
					}
					if f.flusher != nil {
						f.flusher.Flush()
					}
					return len(p), nil
				}
				replaced := providererr.EncodeSSEError(f.pub, f.dialect)
				if f.onRewrite != nil {
					f.onRewrite(string(f.buf))
				}
				f.buf = f.buf[:0]
				if _, err := f.dst.Write(replaced); err != nil {
					return len(p), err
				}
				if f.flusher != nil {
					f.flusher.Flush()
				}
			}
			return len(p), nil
		}
		frame := f.buf[:idx+2]
		f.buf = f.buf[idx+2:]
		out, rewritten := providererr.RewriteSSEFrame(frame, f.pub, f.dialect)
		if rewritten && f.onRewrite != nil {
			f.onRewrite(string(frame))
		}
		if _, err := f.dst.Write(out); err != nil {
			return len(p), err
		}
		if f.flusher != nil {
			f.flusher.Flush()
		}
	}
}

// bypassWrite streams bypass bytes straight to the client, holding back one
// trailing '\n' so a "\n\n" terminator split across writes is still detected.
// It returns how many bytes were consumed; finding the terminator writes it
// and ends bypass mode. The EARLIEST terminator in the chunk ends the frame —
// anything after it is a new frame that must go back through normal
// inspection, never ride along inside the bypassed one.
func (f *ssePublicFilter) bypassWrite(p []byte) (int, error) {
	if f.pendNL {
		f.pendNL = false
		if len(p) > 0 && p[0] == '\n' {
			// The held newline pairs with this one: the frame ended. Write the
			// full terminator and resume normal frame inspection.
			f.bypass = false
			if _, err := f.dst.Write([]byte{'\n', '\n'}); err != nil {
				return 0, err
			}
			return 1, nil
		}
		// The held newline was just a newline inside the frame.
		if _, err := f.dst.Write([]byte{'\n'}); err != nil {
			return 0, err
		}
	}
	if i := bytes.Index(p, []byte("\n\n")); i >= 0 {
		if _, err := f.dst.Write(p[:i+2]); err != nil {
			return 0, err
		}
		f.bypass = false
		return i + 2, nil
	}
	// No terminator in this chunk: everything is frame content except a
	// trailing '\n' that might be a terminator's first half.
	if len(p) > 0 && p[len(p)-1] == '\n' {
		if _, err := f.dst.Write(p[:len(p)-1]); err != nil {
			return 0, err
		}
		f.pendNL = true
		return len(p), nil
	}
	_, err := f.dst.Write(p)
	return len(p), err
}

// isKnownGiantContentFrame reports whether an oversized frame's head names one
// of the Responses content events — the two frame types that legitimately grow
// past any frame buffer because they carry the full generated output. The
// event name is read from the frame's first line (event: form) or the first
// root key of its data payload (data-only form); both are present within the
// first bytes of the frame, so a bounded look suffices no matter how long the
// frame grows. Everything else — error frames, unknown shapes, garbage —
// returns false and keeps the replacement behavior.
func isKnownGiantContentFrame(head []byte) bool {
	bounded := head
	if len(bounded) > 4096 {
		bounded = bounded[:4096]
	}
	if rest, ok := bytes.CutPrefix(bounded, []byte("event:")); ok {
		name := string(bytes.TrimSpace(bytes.SplitN(rest, []byte("\n"), 2)[0]))
		return name == "response.completed" || name == "response.incomplete"
	}
	rest, ok := bytes.CutPrefix(bounded, []byte("data:"))
	if !ok {
		return false
	}
	payload := bytes.TrimSpace(rest)
	switch probeRootType(payload) {
	case "response.completed", "response.incomplete":
		return true
	default:
		return false
	}
}

func (f *ssePublicFilter) Close() error {
	if f.bypass {
		// A bypassed frame that never saw its terminator: flush the held
		// newline and stop. Nothing buffered remains (bypass retains nothing).
		f.bypass = false
		if f.pendNL {
			f.pendNL = false
			if _, err := f.dst.Write([]byte{'\n'}); err != nil {
				return err
			}
		}
		if f.flusher != nil {
			f.flusher.Flush()
		}
		return nil
	}
	if len(f.buf) == 0 {
		return nil
	}
	out, rewritten := providererr.RewriteSSEFrame(append(f.buf, '\n', '\n'), f.pub, f.dialect)
	if rewritten && f.onRewrite != nil {
		f.onRewrite(string(f.buf))
	}
	f.buf = f.buf[:0]
	_, err := f.dst.Write(out)
	if f.flusher != nil {
		f.flusher.Flush()
	}
	return err
}
