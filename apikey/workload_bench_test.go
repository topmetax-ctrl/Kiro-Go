package apikey

import (
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"
)

func benchSeedEvents(b *testing.B, s *Service, keyID string, n int) {
	b.Helper()
	for i := 0; i < n; i++ {
		st := OutcomeSuccess
		if i%17 == 0 {
			st = OutcomeFailed
		}
		if err := s.Commit(keyID, CommitInput{
			RequestID: fmt.Sprintf("b-%d", i), Outcome: st, Endpoint: "openai",
			ClientModel: "claude-sonnet-4.5", EffectiveModel: "claude-sonnet-4.5",
			InputTokens: 3, OutputTokens: 1, StatusCode: 200,
		}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCommitSerial(b *testing.B) {
	s, secrets := benchService(b, 1)
	defer s.Close()
	rec, err := s.Lookup(secrets[0])
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.Commit(rec.Key.ID, CommitInput{
			RequestID: fmt.Sprintf("c-%d", i), Outcome: OutcomeSuccess,
			Endpoint: "openai", StatusCode: 200,
		}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCommitParallel(b *testing.B) {
	s, secrets := benchService(b, 1)
	defer s.Close()
	rec, err := s.Lookup(secrets[0])
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			i++
			if err := s.Commit(rec.Key.ID, CommitInput{
				RequestID: fmt.Sprintf("p-%d-%d", i, time.Now().UnixNano()),
				Outcome:   OutcomeSuccess, Endpoint: "openai", StatusCode: 200,
			}); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func portalBenchSeed() int {
	if v := os.Getenv("APIKEY_BENCH_EVENTS"); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil && n > 0 {
			return n
		}
	}
	return 5000
}

func BenchmarkPortalQueries(b *testing.B) {
	s, secrets := benchService(b, 1)
	defer s.Close()
	rec, err := s.Lookup(secrets[0])
	if err != nil {
		b.Fatal(err)
	}
	n := portalBenchSeed()
	benchSeedEvents(b, s, rec.Key.ID, n)
	q := EventQuery{Limit: 50}
	from := time.Now().UTC().Add(-24 * time.Hour)
	to := time.Now().UTC()
	q.From, q.To = &from, &to
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		page, err := s.ListEvents(rec.Key.ID, q)
		if err != nil {
			b.Fatal(err)
		}
		if page.HasMore {
			q2 := q
			q2.Cursor = page.NextCursor
			if _, err := s.ListEvents(rec.Key.ID, q2); err != nil {
				b.Fatal(err)
			}
		}
		if _, err := s.UsageSeries(rec.Key.ID, q); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSSEPublish(b *testing.B) {
	for _, clients := range []int{10, 100} {
		b.Run(fmt.Sprintf("subs_%d", clients), func(b *testing.B) {
			s, secrets := benchService(b, 1)
			defer s.Close()
			rec, err := s.Lookup(secrets[0])
			if err != nil {
				b.Fatal(err)
			}
			cancels := make([]func(), 0, clients)
			for i := 0; i < clients; i++ {
				ch, cancel := s.SubscribePortal(rec.Key.ID)
				cancels = append(cancels, cancel)
				go func(ch <-chan PublicEvent) {
					for range ch {
					}
				}(ch)
			}
			defer func() {
				for _, c := range cancels {
					c()
				}
			}()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := s.Commit(rec.Key.ID, CommitInput{
					RequestID: fmt.Sprintf("sse-%d", i), Outcome: OutcomeSuccess,
					Endpoint: "openai", StatusCode: 200,
				}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
