package search

import (
	"context"
	"testing"

	"kiro-go/config"
)

func BenchmarkRoute(b *testing.B) {
	primary := &scriptedProvider{name: config.ProviderSearXNG, resp: Response{Results: goodResults(5)}, state: ProviderHealthy}
	router := newProviderRouter([]providerEntry{{provider: primary}}, nil, false)
	ctx := context.Background()
	req := Request{Query: "golang concurrency", MaxResults: 5}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, _ = router.Route(ctx, req)
	}
}
