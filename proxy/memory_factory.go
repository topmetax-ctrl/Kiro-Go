package proxy

import (
	"context"

	"kiro-go/config"
	"kiro-go/logger"
)

// newMemoryProviderFromConfig builds the memory provider from the resolved
// memory config. It returns a noopMemoryProvider (never nil) when memory is
// disabled or unconfigured, so callers never nil-check. When enabled, it wraps
// the Mem0 backend in a fail-open decorator (unless the operator turned fail-open
// off) so a memory outage degrades silently instead of breaking the caller.
//
// Mirrors newSearchOrchestratorFromConfig's "return a null-object when
// unconfigured" contract, and is rebuilt on config change (see
// Handler.rebuildMemoryProvider) the same way the account pool is reloaded.
func newMemoryProviderFromConfig() MemoryProvider {
	if !config.MemoryEnabled() {
		return noopMemoryProvider{}
	}
	m := config.GetMemoryConfig()

	switch m.Provider {
	case config.DefaultMemoryProvider, "": // "mem0"
		p, err := NewMem0HTTPProvider(m.BaseURL)
		if err != nil {
			logger.Warnf("[Memory] disabled: %v", err)
			return noopMemoryProvider{}
		}
		logger.Infof("[Memory] provider ready: mem0 writeMode=%s failOpen=%v redactSecrets=%v storeSourceCode=%v",
			m.WriteMode, config.MemoryFailOpen(), config.MemoryRedactSecrets(), m.Redaction.StoreSourceCode)
		if config.MemoryFailOpen() {
			return failOpenMemoryProvider{inner: p}
		}
		return p
	default:
		logger.Warnf("[Memory] disabled: unknown provider %q", m.Provider)
		return noopMemoryProvider{}
	}
}

// failOpenMemoryProvider decorates a MemoryProvider so that recall (Search) and
// capture (Add) never surface backend errors to the caller: a memory outage must
// not break the request path. Delete and Health stay honest — a silently
// swallowed delete failure would mislead an operator into thinking data was
// purged, and Health exists precisely to report backend trouble.
type failOpenMemoryProvider struct {
	inner MemoryProvider
}

func (f failOpenMemoryProvider) Search(ctx context.Context, q SearchQuery) ([]Memory, error) {
	mems, err := f.inner.Search(ctx, q)
	if err != nil {
		// Context cancellation is the caller's own signal — surface it, don't mask it.
		if ctx.Err() != nil {
			return nil, err
		}
		logger.Warnf("[Memory] search failed (fail-open, returning no memories): %v", err)
		return nil, nil
	}
	return mems, nil
}

func (f failOpenMemoryProvider) Add(ctx context.Context, in AddMemoryInput) error {
	if err := f.inner.Add(ctx, in); err != nil {
		if ctx.Err() != nil {
			return err
		}
		logger.Warnf("[Memory] add failed (fail-open, dropped): %v", err)
		return nil
	}
	return nil
}

func (f failOpenMemoryProvider) Delete(ctx context.Context, scope MemoryScope) error {
	return f.inner.Delete(ctx, scope)
}

func (f failOpenMemoryProvider) Health(ctx context.Context) error {
	return f.inner.Health(ctx)
}

func (f failOpenMemoryProvider) Name() string { return f.inner.Name() }
