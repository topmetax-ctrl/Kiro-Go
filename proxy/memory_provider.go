package proxy

import "context"

// MemoryScope identifies the owner a memory belongs to. For now it carries only
// the caller principal (config.ApiKeyEntry.ID, or "anonymous" when auth is off),
// resolved via responsesOwnerPrincipal — the same identity the responses store
// uses. Additional dimensions (workspace, project, agent) may be added later
// without breaking the interface.
type MemoryScope struct {
	Principal string
}

// Memory is a single stored memory returned by a search.
type Memory struct {
	ID       string         `json:"id"`
	Text     string         `json:"text"`
	Score    float64        `json:"score,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// MemoryMessage is one turn of the conversation being persisted.
type MemoryMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// SearchQuery is a retrieval request scoped to a single owner.
type SearchQuery struct {
	Scope MemoryScope
	Query string
	Limit int
}

// AddMemoryInput is a store request scoped to a single owner.
type AddMemoryInput struct {
	Scope    MemoryScope
	Messages []MemoryMessage
}

// MemoryProvider is a long-term memory backend. Mem0 (self-hosted, HTTP)
// implements it. Implementations must:
//   - honor ctx cancellation,
//   - return a *MemoryProviderError (typed kind) on backend failure,
//   - never log the backend API key,
//   - never persist content the redaction policy forbids.
type MemoryProvider interface {
	Search(ctx context.Context, q SearchQuery) ([]Memory, error)
	Add(ctx context.Context, in AddMemoryInput) error
	Delete(ctx context.Context, scope MemoryScope) error
	Health(ctx context.Context) error
	Name() string
}

// noopMemoryProvider is the null-object returned when memory is disabled or
// unconfigured, so callers never nil-check. All operations succeed as no-ops;
// Search returns no memories. Mirrors noopSearchCache.
type noopMemoryProvider struct{}

func (noopMemoryProvider) Search(context.Context, SearchQuery) ([]Memory, error) { return nil, nil }
func (noopMemoryProvider) Add(context.Context, AddMemoryInput) error             { return nil }
func (noopMemoryProvider) Delete(context.Context, MemoryScope) error             { return nil }
func (noopMemoryProvider) Health(context.Context) error                          { return nil }
func (noopMemoryProvider) Name() string                                          { return "noop" }
