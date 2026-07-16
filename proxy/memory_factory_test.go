package proxy

import (
	"context"
	"errors"
	"testing"
)

// stubMemoryProvider lets tests drive the fail-open decorator with controlled
// errors per method.
type stubMemoryProvider struct {
	searchErr error
	addErr    error
	deleteErr error
	healthErr error
}

func (s stubMemoryProvider) Search(context.Context, SearchQuery) ([]Memory, error) {
	if s.searchErr != nil {
		return nil, s.searchErr
	}
	return []Memory{{ID: "m1"}}, nil
}
func (s stubMemoryProvider) Add(context.Context, AddMemoryInput) error { return s.addErr }
func (s stubMemoryProvider) Delete(context.Context, MemoryScope) error { return s.deleteErr }
func (s stubMemoryProvider) Health(context.Context) error             { return s.healthErr }
func (s stubMemoryProvider) Name() string                             { return "stub" }

func TestFailOpenSearchSwallowsBackendError(t *testing.T) {
	f := failOpenMemoryProvider{inner: stubMemoryProvider{searchErr: errors.New("backend down")}}
	mems, err := f.Search(context.Background(), SearchQuery{Scope: MemoryScope{Principal: "u1"}, Query: "x"})
	if err != nil {
		t.Fatalf("fail-open search must not surface backend error, got %v", err)
	}
	if mems != nil {
		t.Errorf("expected no memories on failure, got %+v", mems)
	}
}

func TestFailOpenSearchSurfacesContextCancel(t *testing.T) {
	f := failOpenMemoryProvider{inner: stubMemoryProvider{searchErr: context.Canceled}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := f.Search(ctx, SearchQuery{Scope: MemoryScope{Principal: "u1"}, Query: "x"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("context cancellation must surface, got %v", err)
	}
}

func TestFailOpenAddSwallowsBackendError(t *testing.T) {
	f := failOpenMemoryProvider{inner: stubMemoryProvider{addErr: errors.New("backend down")}}
	err := f.Add(context.Background(), AddMemoryInput{Scope: MemoryScope{Principal: "u1"}, Messages: []MemoryMessage{{Role: "user", Content: "x"}}})
	if err != nil {
		t.Fatalf("fail-open add must not surface backend error, got %v", err)
	}
}

func TestFailOpenDeleteStaysHonest(t *testing.T) {
	sentinel := errors.New("delete failed")
	f := failOpenMemoryProvider{inner: stubMemoryProvider{deleteErr: sentinel}}
	err := f.Delete(context.Background(), MemoryScope{Principal: "u1"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("delete must surface errors (not fail-open), got %v", err)
	}
}

func TestFailOpenHealthStaysHonest(t *testing.T) {
	sentinel := errors.New("unhealthy")
	f := failOpenMemoryProvider{inner: stubMemoryProvider{healthErr: sentinel}}
	if err := f.Health(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("health must surface errors, got %v", err)
	}
}
