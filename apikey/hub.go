package apikey

import (
	"sync"
	"sync/atomic"
)

type portalSub struct {
	keyID  string
	ch     chan PublicEvent
	closed atomic.Bool
}

type portalHub struct {
	mu   sync.Mutex
	subs map[*portalSub]struct{}
}

func newPortalHub() *portalHub {
	return &portalHub{subs: make(map[*portalSub]struct{})}
}

// SubscribePortal starts buffering persisted public events for one key.
// The channel is bounded; a slow consumer is disconnected instead of
// blocking Commit.
func (s *Service) SubscribePortal(keyID string) (<-chan PublicEvent, func()) {
	if s == nil || s.hub == nil || keyID == "" {
		ch := make(chan PublicEvent)
		close(ch)
		return ch, func() {}
	}
	sub := &portalSub{
		keyID: keyID,
		ch:    make(chan PublicEvent, portalSubBuffer),
	}
	s.hub.mu.Lock()
	s.hub.subs[sub] = struct{}{}
	s.hub.mu.Unlock()
	IncPortalSSEConn()
	var once sync.Once
	cancel := func() {
		once.Do(func() { s.hub.unsubscribe(sub) })
	}
	return sub.ch, cancel
}

func (h *portalHub) unsubscribe(sub *portalSub) {
	if h == nil || sub == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.removeLocked(sub)
}

func (h *portalHub) removeLocked(sub *portalSub) {
	if _, ok := h.subs[sub]; !ok {
		return
	}
	delete(h.subs, sub)
	if sub.closed.CompareAndSwap(false, true) {
		close(sub.ch)
		DecPortalSSEConn()
	}
}

func (s *Service) publishPortal(ev PublicEvent) {
	if s == nil || s.hub == nil || ev.ApiKeyID == "" {
		return
	}
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	var slow []*portalSub
	for sub := range s.hub.subs {
		if sub.keyID != ev.ApiKeyID {
			continue
		}
		select {
		case sub.ch <- ev:
		default:
			slow = append(slow, sub)
		}
	}
	for _, sub := range slow {
		s.hub.removeLocked(sub)
		IncPortalSSESlow()
	}
}

// PortalStream is the replay/live handoff for one SSE connection.
type PortalStream struct {
	Replay      []PublicEvent
	Live        <-chan PublicEvent
	Cancel      func()
	HighWater   int64
	LastEventID int64
	Truncated   bool
}

// OpenPortalStream implements subscribe-first handoff:
//
//	subscribe (buffer live) → capture high-water → replay (lastID, highWater]
//	→ caller drains the buffer and continues live.
//
// lastEventID == 0 means first connect: no historical dump, live only.
// Events committed after subscribe cannot be lost; overlap is deduped by eventId.
func (s *Service) OpenPortalStream(keyID string, lastEventID int64, filter EventQuery) (*PortalStream, error) {
	if s == nil || keyID == "" {
		return nil, ErrNotFound
	}
	live, cancel := s.SubscribePortal(keyID)
	if s.handoffHook != nil {
		s.handoffHook("subscribed")
	}
	hw, err := s.EventHighWater(keyID)
	if err != nil {
		cancel()
		return nil, err
	}
	if s.handoffHook != nil {
		s.handoffHook("watermarked")
	}
	var replay []PublicEvent
	var truncated bool
	if lastEventID > 0 && hw > lastEventID {
		replay, truncated, err = s.ReplayEvents(keyID, lastEventID, hw, filter, MaxEventReplay)
		if err != nil {
			cancel()
			return nil, err
		}
		IncPortalReplay(len(replay))
		if truncated {
			IncPortalReplayTruncated()
		}
	}
	if s.handoffHook != nil {
		s.handoffHook("replayed")
	}
	return &PortalStream{
		Replay:      replay,
		Live:        live,
		Cancel:      cancel,
		HighWater:   hw,
		LastEventID: lastEventID,
		Truncated:   truncated,
	}, nil
}

// SetHandoffHookForTest injects a callback between handoff phases so tests
// can commit events in the classic race windows. Production code must not
// call this.
func (s *Service) SetHandoffHookForTest(fn func(phase string)) {
	s.handoffHook = fn
}

func (s *Service) EventHighWater(keyID string) (int64, error) {
	if keyID == "" {
		return 0, ErrNotFound
	}
	var id int64
	err := s.db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM request_events WHERE key_id=?`, keyID).Scan(&id)
	return id, err
}

// ReplayEvents returns persisted events in (afterID, throughID] in id order.
// limit+1 detects overflow: Truncated means the matching backlog exceeded the
// replay cap. A filter that matches fewer rows than the cap is not truncated
// even if unfiltered ids jumped by more than the cap.
func (s *Service) ReplayEvents(keyID string, afterID, throughID int64, q EventQuery, limit int) ([]PublicEvent, bool, error) {
	if keyID == "" {
		return nil, false, ErrNotFound
	}
	if limit <= 0 || limit > MaxEventReplay {
		limit = MaxEventReplay
	}
	where, args := q.where(keyID)
	where += " AND id > ? AND id <= ?"
	args = append(args, afterID, throughID)
	rows, err := s.db.Query(`SELECT `+eventSelectCols+` FROM request_events WHERE `+where+` ORDER BY id ASC LIMIT ?`,
		append(args, limit+1)...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	events, err := scanEvents(rows, keyID)
	if err != nil {
		return nil, false, err
	}
	truncated := len(events) > limit
	if truncated {
		events = events[:limit]
	}
	return events, truncated, nil
}
