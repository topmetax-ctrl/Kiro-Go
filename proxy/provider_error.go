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
type ssePublicFilter struct {
	dst       io.Writer
	flusher   http.Flusher
	dialect   providererr.Dialect
	pub       providererr.PublicError
	onRewrite func(raw string)
	buf       []byte
}

func (f *ssePublicFilter) Write(p []byte) (int, error) {
	f.buf = append(f.buf, p...)
	for {
		idx := bytes.Index(f.buf, []byte("\n\n"))
		if idx < 0 {
			if len(f.buf) > providererr.MaxParseBytes {
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

func (f *ssePublicFilter) Close() error {
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
