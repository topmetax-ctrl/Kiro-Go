package apikey

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func collectLive(ch <-chan PublicEvent, n int, d time.Duration) []PublicEvent {
	deadline := time.After(d)
	var out []PublicEvent
	for len(out) < n {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			return out
		}
	}
	return out
}

func TestHandoffNoGapNoDuplicate(t *testing.T) {
	ResetObservabilityForTest()
	s := testService(t)
	rec, _, err := s.Create(CreateInput{Name: "h", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if err := s.Commit(rec.Key.ID, CommitInput{
			RequestID: fmt.Sprintf("seed-%d", i), Outcome: OutcomeSuccess,
			Endpoint: "openai", StatusCode: 200,
		}); err != nil {
			t.Fatal(err)
		}
	}
	hw, _ := s.EventHighWater(rec.Key.ID)
	lastID := hw - 2

	s.SetHandoffHookForTest(func(phase string) {
		switch phase {
		case "subscribed":
			_ = s.Commit(rec.Key.ID, CommitInput{RequestID: "during-sub", Outcome: OutcomeSuccess, Endpoint: "openai", StatusCode: 200})
		case "watermarked":
			_ = s.Commit(rec.Key.ID, CommitInput{RequestID: "during-water", Outcome: OutcomeSuccess, Endpoint: "openai", StatusCode: 200})
		}
	})
	stream, err := s.OpenPortalStream(rec.Key.ID, lastID, EventQuery{})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Cancel()

	seen := map[string]int{}
	note := func(ev PublicEvent) {
		seen[ev.RequestID]++
	}
	for _, ev := range stream.Replay {
		note(ev)
	}
	live := collectLive(stream.Live, 8, time.Second)
	for _, ev := range live {
		if ev.EventID != 0 && seen[ev.RequestID] > 0 {
			continue
		}
		note(ev)
	}
	// Drain remaining overlap with a short wait.
	for _, ev := range collectLive(stream.Live, 4, 50*time.Millisecond) {
		note(ev)
	}

	want := []string{"seed-6", "seed-7", "during-sub", "during-water"}
	for _, id := range want {
		if seen[id] == 0 {
			t.Fatalf("missing %s in %+v replay=%d live=%d hw=%d", id, seen, len(stream.Replay), len(live), stream.HighWater)
		}
	}
	byEvent := map[int64]int{}
	for _, ev := range stream.Replay {
		byEvent[ev.EventID]++
	}
	for _, ev := range live {
		byEvent[ev.EventID]++
	}
	// Presentation layer: unique event IDs after union+dedupe.
	uniq := map[int64]struct{}{}
	add := func(ev PublicEvent) {
		if ev.EventID != 0 {
			uniq[ev.EventID] = struct{}{}
		}
	}
	for _, ev := range stream.Replay {
		add(ev)
	}
	for _, ev := range append(live, collectLive(stream.Live, 4, 20*time.Millisecond)...) {
		add(ev)
	}
	if UnsettledReservations() != 0 {
		t.Fatalf("unsettled %d", UnsettledReservations())
	}
}

func TestFirstConnectLiveOnlyNoHistoryDump(t *testing.T) {
	s := testService(t)
	rec, _, _ := s.Create(CreateInput{Name: "f", Enabled: true})
	_ = s.Commit(rec.Key.ID, CommitInput{RequestID: "old", Outcome: OutcomeSuccess, Endpoint: "openai", StatusCode: 200})
	s.SetHandoffHookForTest(func(phase string) {
		if phase == "subscribed" {
			_ = s.Commit(rec.Key.ID, CommitInput{RequestID: "live-a", Outcome: OutcomeSuccess, Endpoint: "openai", StatusCode: 200})
		}
		if phase == "watermarked" {
			_ = s.Commit(rec.Key.ID, CommitInput{RequestID: "live-b", Outcome: OutcomeSuccess, Endpoint: "openai", StatusCode: 200})
		}
	})
	stream, err := s.OpenPortalStream(rec.Key.ID, 0, EventQuery{})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Cancel()
	if len(stream.Replay) != 0 {
		t.Fatalf("first connect must not dump history: %+v", stream.Replay)
	}
	got := map[string]bool{}
	for _, ev := range collectLive(stream.Live, 2, time.Second) {
		got[ev.RequestID] = true
	}
	if !got["live-a"] || !got["live-b"] {
		t.Fatalf("lost live during first connect: %+v", got)
	}
	if got["old"] {
		t.Fatal("first connect replayed history")
	}
}

func TestPortalSubscribeIsolation(t *testing.T) {
	s := testService(t)
	a, _, _ := s.Create(CreateInput{Name: "a", Enabled: true})
	b, _, _ := s.Create(CreateInput{Name: "b", Enabled: true})
	chA, cancelA := s.SubscribePortal(a.Key.ID)
	chB, cancelB := s.SubscribePortal(b.Key.ID)
	defer cancelA()
	defer cancelB()
	_ = s.Commit(a.Key.ID, CommitInput{RequestID: "only-a", Outcome: OutcomeSuccess, Endpoint: "openai", StatusCode: 200})
	_ = s.Commit(b.Key.ID, CommitInput{RequestID: "only-b", Outcome: OutcomeSuccess, Endpoint: "openai", StatusCode: 200})
	gotA := collectLive(chA, 1, time.Second)
	gotB := collectLive(chB, 1, time.Second)
	if len(gotA) != 1 || gotA[0].RequestID != "only-a" {
		t.Fatalf("A: %+v", gotA)
	}
	if len(gotB) != 1 || gotB[0].RequestID != "only-b" {
		t.Fatalf("B: %+v", gotB)
	}
}

func TestSlowClientDisconnectsWithoutBlockingCommit(t *testing.T) {
	ResetObservabilityForTest()
	s := testService(t)
	rec, _, _ := s.Create(CreateInput{Name: "slow", Enabled: true})
	ch, cancel := s.SubscribePortal(rec.Key.ID)
	defer cancel()
	done := make(chan struct{})
	go func() {
		for i := 0; i < portalSubBuffer+8; i++ {
			if err := s.Commit(rec.Key.ID, CommitInput{
				RequestID: fmt.Sprintf("flood-%d", i), Outcome: OutcomeSuccess,
				Endpoint: "openai", StatusCode: 200,
			}); err != nil {
				t.Errorf("commit blocked or failed: %v", err)
				break
			}
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("slow subscriber blocked Commit")
	}
	if PortalSSESlowClients() < 1 {
		t.Fatalf("expected slow-client counter, got %d", PortalSSESlowClients())
	}
	// Channel must close (drop) rather than stay open and stall.
	closed := false
	deadline := time.After(time.Second)
	for !closed {
		select {
		case _, ok := <-ch:
			if !ok {
				closed = true
			}
		case <-deadline:
			t.Fatal("slow subscriber was not disconnected")
		}
	}
}

func TestSubscriberCleanup(t *testing.T) {
	ResetObservabilityForTest()
	s := testService(t)
	rec, _, _ := s.Create(CreateInput{Name: "c", Enabled: true})
	_, cancel := s.SubscribePortal(rec.Key.ID)
	if PortalSSEConnectionsActive() != 1 {
		t.Fatalf("active %d", PortalSSEConnectionsActive())
	}
	cancel()
	cancel() // idempotent
	if PortalSSEConnectionsActive() != 0 {
		t.Fatalf("leak active=%d", PortalSSEConnectionsActive())
	}
	_ = s.Commit(rec.Key.ID, CommitInput{RequestID: "after", Outcome: OutcomeSuccess, Endpoint: "openai", StatusCode: 200})
}

func TestReplayAfterRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "apikeys.db")
	pepper := []byte("test-pepper-32-bytes-long!!!!!!")
	s1, err := Open(path, pepper, Options{})
	if err != nil {
		t.Fatal(err)
	}
	rec, _, err := s1.Create(CreateInput{Name: "r", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		_ = s1.Commit(rec.Key.ID, CommitInput{RequestID: fmt.Sprintf("p-%d", i), Outcome: OutcomeSuccess, Endpoint: "openai", StatusCode: 200})
	}
	hw, _ := s1.EventHighWater(rec.Key.ID)
	_ = s1.Close()

	s2, err := Open(path, pepper, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	stream, err := s2.OpenPortalStream(rec.Key.ID, hw-2, EventQuery{})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Cancel()
	if len(stream.Replay) < 2 {
		t.Fatalf("restart replay %d", len(stream.Replay))
	}
	ids := map[string]bool{}
	for _, ev := range stream.Replay {
		ids[ev.RequestID] = true
	}
	if !ids["p-1"] || !ids["p-2"] {
		t.Fatalf("replay after restart: %+v", ids)
	}
}

func TestHandoffConcurrentCommits(t *testing.T) {
	s := testService(t)
	rec, _, _ := s.Create(CreateInput{Name: "race", Enabled: true})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = s.Commit(rec.Key.ID, CommitInput{
				RequestID: fmt.Sprintf("c-%d", i), Outcome: OutcomeSuccess,
				Endpoint: "openai", StatusCode: 200,
			})
		}(i)
	}
	stream, err := s.OpenPortalStream(rec.Key.ID, 0, EventQuery{})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Cancel()
	wg.Wait()
	// Live may have a subset (those after subscribe). History API has all 20.
	page, err := s.ListEvents(rec.Key.ID, EventQuery{Limit: 50})
	if err != nil || len(page.Items) != 20 {
		t.Fatalf("history %d err=%v", len(page.Items), err)
	}
	seen := map[int64]struct{}{}
	for _, ev := range stream.Replay {
		seen[ev.EventID] = struct{}{}
	}
	for _, ev := range collectLive(stream.Live, 20, 200*time.Millisecond) {
		if _, ok := seen[ev.EventID]; ok && ev.EventID != 0 {
			continue
		}
		seen[ev.EventID] = struct{}{}
	}
}

func TestRevokeClosesSession(t *testing.T) {
	s := testService(t)
	rec, secret, _ := s.Create(CreateInput{Name: "v", Enabled: true})
	sid, _, err := s.OpenSessionByKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := s.CreatePortalToken(rec.Key.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.OpenSessionByPortalToken(tok); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokePortalToken(rec.Key.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionRecord(sid); err != ErrSession {
		t.Fatalf("revoked token must invalidate sessions: %v", err)
	}
	if _, _, err := s.OpenSessionByPortalToken(tok); err == nil {
		t.Fatal("revoked token still opened")
	}
}

func TestDisabledKeyKillsSession(t *testing.T) {
	s := testService(t)
	rec, secret, _ := s.Create(CreateInput{Name: "d", Enabled: true})
	sid, _, _ := s.OpenSessionByKey(secret)
	off := false
	if _, err := s.Update(rec.Key.ID, UpdateInput{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionRecord(sid); err != ErrSession {
		t.Fatalf("disabled key session: %v", err)
	}
}
