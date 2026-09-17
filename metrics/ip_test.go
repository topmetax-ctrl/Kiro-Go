package metrics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// ipEv builds an event attributed to one source IP; the IP rollup only reads
// outcome, tokens, latency and timestamp.
func ipEv(ip string, ok bool, latency int64) Event {
	e := ev("p1", ok, 200, latency)
	e.ClientIP = ip
	return e
}

func TestTopIPsRanksByRequests(t *testing.T) {
	reset(t)
	for i := 0; i < 3; i++ {
		Record(ipEv("10.0.0.3", true, 100))
	}
	for i := 0; i < 5; i++ {
		Record(ipEv("10.0.0.1", true, 200))
	}
	Record(ipEv("10.0.0.2", false, 300))

	ips := TopIPs(0)
	if len(ips) != 3 {
		t.Fatalf("len = %d, want 3", len(ips))
	}
	if ips[0].IP != "10.0.0.1" || ips[0].Requests != 5 {
		t.Fatalf("top = %+v, want 10.0.0.1 with 5 requests", ips[0])
	}
	// Success rate inputs survive the rollup: 5 ok vs 1 failed.
	if ips[0].Success != 5 || ips[2].Failed != 1 {
		t.Fatalf("outcomes = %+v / %+v", ips[0], ips[2])
	}
	// A positive limit truncates the leaderboard.
	if got := TopIPs(2); len(got) != 2 {
		t.Fatalf("limit=2 returned %d rows", len(got))
	}
}

func TestTopIPsWindowScopesToHours(t *testing.T) {
	reset(t)
	recent := ipEv("10.0.0.1", true, 100)
	Record(recent)

	old := ipEv("10.0.0.2", true, 100)
	old.TimeMs = nowMs() - 3*3600000 // three hours ago
	Record(old)

	// All-time keeps both callers.
	if got := TopIPsWindow(0, 0); len(got) != 2 {
		t.Fatalf("all-time = %d rows, want 2", len(got))
	}
	// The 1h window reads the minute series: only the recent caller remains,
	// and its totals come from the bucket rather than the lifetime counter.
	got := TopIPsWindow(1, 0)
	if len(got) != 1 || got[0].IP != "10.0.0.1" {
		t.Fatalf("1h window = %+v, want only 10.0.0.1", got)
	}
	if got[0].Requests != 1 || got[0].TotalTokens != recent.InputTokens+recent.OutputTokens {
		t.Fatalf("windowed stat = %+v", got[0])
	}
}

func TestTopIPsWindowEmptyForUnknownIPs(t *testing.T) {
	reset(t)
	// An IP whose traffic is entirely outside the window must not appear with
	// zeroed figures — the leaderboard lists callers, not configured entities.
	old := ipEv("10.0.0.9", true, 100)
	old.TimeMs = nowMs() - 48*3600000
	Record(old)
	if got := TopIPsWindow(1, 0); len(got) != 0 {
		t.Fatalf("1h window = %+v, want empty", got)
	}
}

func TestIPPersistRoundTrip(t *testing.T) {
	reset(t)
	for i := 0; i < 2; i++ {
		Record(ipEv("192.168.1.1", true, 500))
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	// A fresh store models a restart: lifetime counters and the hourly rollups
	// must both come back, so the windowed leaderboard survives the process.
	reset(t)
	if err := Load(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	ips := TopIPs(0)
	if len(ips) != 1 || ips[0].IP != "192.168.1.1" || ips[0].Requests != 2 {
		t.Fatalf("restored lifetime = %+v", ips)
	}
	got := TopIPsWindow(24, 0)
	if len(got) != 1 || got[0].Requests != 2 {
		t.Fatalf("restored 24h window = %+v, want 192.168.1.1 with 2 requests", got)
	}
}

func TestIPPersistRoundTripOldFileWithoutHours(t *testing.T) {
	reset(t)
	Record(ipEv("10.1.1.1", true, 100))
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	// Strip the hours arrays to model a state file written before IP history
	// existed: lifetime counters must still load, hours just start empty.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	byIp, _ := m["byIp"].(map[string]any)
	for _, v := range byIp {
		if obj, ok := v.(map[string]any); ok {
			delete(obj, "hours")
		}
	}
	b, err = json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	reset(t)
	if err := Load(path); err != nil {
		t.Fatalf("load: %v", err)
	}
	if ips := TopIPs(0); len(ips) != 1 || ips[0].Requests != 1 {
		t.Fatalf("restored lifetime = %+v", ips)
	}
	if got := TopIPsWindow(24, 0); len(got) != 0 {
		t.Fatalf("window without saved hours = %+v, want empty", got)
	}
}

// TopIPsRange must scope to the requested window the way TopIPsWindow does
// for the hour presets — the callers tab's custom range is inert otherwise.
func TestTopIPsRangeScopesToWindow(t *testing.T) {
	reset(t)
	recent := ipEv("10.0.0.1", true, 100)
	Record(recent)
	old := ipEv("10.0.0.1", true, 100)
	old.TimeMs = nowMs() - 5*3600000
	Record(old)

	got := TopIPsRange(nowMs()-3600000, 0, 50)
	if len(got) != 1 || got[0].Requests != 1 {
		t.Fatalf("1h range = %+v, want 1 request", got)
	}
	got = TopIPsRange(nowMs()-24*3600000, 0, 50)
	if len(got) != 1 || got[0].Requests != 2 {
		t.Fatalf("24h range = %+v, want 2 requests", got)
	}
}
