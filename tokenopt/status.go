package tokenopt

import (
	"kiro-go/config"
)

// Status is the runtime view served to the admin UI. It separates what is
// SAVED (returned elsewhere) from what is actually TRUE right now: whether an
// engine is reachable, whether it is applying anything, and what it cannot do.

// ProtocolCaps states, per request protocol, what the layer really supports, so
// the UI reports a capability instead of silently doing nothing.
type ProtocolCaps struct {
	// Headroom: tool-result compression (Kiro-native and external providers).
	Headroom bool `json:"headroom"`
	// Behavior: Caveman / Ponytail prompt injection (Kiro-native and external providers).
	Behavior bool `json:"behavior"`
}

// HeadroomStatus describes the Headroom adapter.
type HeadroomStatus struct {
	Selected   bool `json:"selected"`
	Configured bool `json:"configured"`
	// State: disabled | unconfigured | unverified | connected | disconnected.
	State string `json:"state"`
	// Active: rewriting requests now. Observing: measuring only.
	Active    bool   `json:"active"`
	Observing bool   `json:"observing"`
	Circuit   string `json:"circuit"` // closed | open | half-open
	// TestedVersion is the release the adapter was verified against.
	TestedVersion string        `json:"testedVersion"`
	LastCheck     *HealthReport `json:"lastCheck,omitempty"`
	MemoEntries   int           `json:"memoEntries"`
	MemoBytes     int           `json:"memoBytes"`
}

// RTKStatus describes RTK. GatewayLevel is always false: RTK has no API the
// gateway can call to compress a request, so selecting it never changes a body.
type RTKStatus struct {
	Selected     bool `json:"selected"`
	GatewayLevel bool `json:"gatewayLevel"`
	// State: client_side (no proxy configured; nothing to verify from here) |
	// unverified | connected | disconnected.
	State           string        `json:"state"`
	ProxyURL        string        `json:"proxyUrl,omitempty"`
	RoutedProviders []string      `json:"routedProviders"`
	LastCheck       *HealthReport `json:"lastCheck,omitempty"`
}

// BehaviorStatus describes Caveman or Ponytail.
type BehaviorStatus struct {
	Level string `json:"level"`
	// State: inactive | active | unavailable (asset failed verification).
	State string `json:"state"`
	// Observable is always false: a prompt's effect on output length cannot be
	// simulated without sending it, so observe mode reports "not observable".
	Observable bool   `json:"observable"`
	Asset      *Asset `json:"asset,omitempty"`
}

// Status is the full runtime view.
type Status struct {
	Mode      string                  `json:"mode"`
	Headroom  HeadroomStatus          `json:"headroom"`
	RTK       RTKStatus               `json:"rtk"`
	Caveman   BehaviorStatus          `json:"caveman"`
	Ponytail  BehaviorStatus          `json:"ponytail"`
	Protocols map[string]ProtocolCaps `json:"protocols"`
	// ExactBytesTools never reach an engine; their output is used verbatim.
	ExactBytesTools []string        `json:"exactBytesTools"`
	Assets          []Asset         `json:"assets"`
	AssetsVerified  bool            `json:"assetsVerified"`
	AssetsError     string          `json:"assetsError,omitempty"`
	Metrics         MetricsSnapshot `json:"metrics"`
}

// Status builds the runtime view. It performs no network I/O: engine health is
// whatever the last verification or real call observed.
func (m *Manager) Status(cfg config.TokenOptimizationConfig, providers []ProviderRef) Status {
	assets, aerr := Assets()
	byID := map[string]Asset{}
	for _, a := range assets {
		if body(a.ID) != "" { // only verified assets are offered
			byID[a.ID] = a
		}
	}
	hHealth, rHealth := m.LastHealth()

	st := Status{
		Mode: cfg.Mode,
		Protocols: map[string]ProtocolCaps{
			string(ProtocolClaude):    {Headroom: true, Behavior: true},
			string(ProtocolOpenAI):    {Headroom: true, Behavior: true},
			string(ProtocolResponses): {Headroom: true, Behavior: true},
		},
		ExactBytesTools: ExactBytesToolNames(),
		Assets:          assets,
		AssetsVerified:  aerr == nil,
		Metrics:         m.mx.snapshot(),
	}
	if aerr != nil {
		st.AssetsError = aerr.Error()
	}

	// Headroom
	hs := HeadroomStatus{
		Selected:      cfg.InputCompression == config.TokenOptInputHeadroom,
		Configured:    cfg.Headroom.BaseURL != "",
		Circuit:       m.br.state(),
		TestedVersion: TestedHeadroomVersion,
		LastCheck:     hHealth,
	}
	hs.MemoEntries, hs.MemoBytes = m.memo.stats()
	switch {
	case !hs.Selected || cfg.Mode == config.TokenOptModeOff:
		hs.State = "disabled"
	case !hs.Configured:
		hs.State = "unconfigured"
	case hHealth == nil:
		hs.State = "unverified"
	case hHealth.Reachable && hHealth.CompressOK:
		hs.State = "connected"
	default:
		hs.State = "disconnected"
	}
	usable := hs.State == "connected" || hs.State == "unverified"
	hs.Active = cfg.Mode == config.TokenOptModeOn && usable && hs.Circuit != "open"
	hs.Observing = cfg.Mode == config.TokenOptModeObserve && hs.Selected && usable && hs.Circuit != "open"
	st.Headroom = hs

	// RTK
	rs := RTKStatus{
		Selected:        cfg.InputCompression == config.TokenOptInputRTK,
		GatewayLevel:    false,
		ProxyURL:        cfg.RTK.ProxyURL,
		RoutedProviders: RoutedProviders(cfg.RTK.ProxyURL, providers),
		LastCheck:       rHealth,
	}
	switch {
	case cfg.RTK.ProxyURL == "":
		rs.State = "client_side"
	case rHealth == nil:
		rs.State = "unverified"
	case rHealth.Reachable:
		rs.State = "connected"
	default:
		rs.State = "disconnected"
	}
	st.RTK = rs

	// Caveman / Ponytail
	behavior := func(level, off string, assetID func(string) string) BehaviorStatus {
		bs := BehaviorStatus{Level: level, State: "inactive"}
		if level == off {
			return bs
		}
		a, ok := byID[assetID(level)]
		if !ok {
			bs.State = "unavailable"
			return bs
		}
		bs.Asset = &a
		if cfg.Mode == config.TokenOptModeOn {
			bs.State = "active"
		}
		return bs
	}
	st.Caveman = behavior(cfg.Caveman, config.TokenOptStyleOff, func(l string) string {
		if l == config.TokenOptUltracave {
			return "caveman.ultracave"
		}
		return "caveman.caveman"
	})
	st.Ponytail = behavior(cfg.Ponytail, config.TokenOptPonytailOff, func(string) string { return "ponytail.skill" })
	return st
}
