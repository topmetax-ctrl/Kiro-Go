package config

import (
	"regexp"
	"strconv"
	"strings"
)

// ConnectionStrategy values stored on UpstreamProvider.ConnectionStrategy.
const (
	ConnectionStrategyPrimary    = "primary"
	ConnectionStrategyRoundRobin = "round_robin"

	maxConnectionsPerProvider = 1000
)

// UpstreamConnection is one API-key credential belonging to an UpstreamProvider.
// Identity lives on ID so rename/reorder/disable never lose the stored secret.
type UpstreamConnection struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	ApiKey  string `json:"apiKey,omitempty"`
	Enabled bool   `json:"enabled"`
	Weight  int    `json:"weight,omitempty"`
}

var keyNNameRE = regexp.MustCompile(`^Key (\d+)$`)

// cloneUpstreamProviders deep-copies providers so callers cannot mutate the
// shared Connections slice without going through the config lock.
func cloneUpstreamProviders(in []UpstreamProvider) []UpstreamProvider {
	if in == nil {
		return nil
	}
	out := make([]UpstreamProvider, len(in))
	for i, p := range in {
		out[i] = p
		if p.Connections != nil {
			out[i].Connections = append([]UpstreamConnection(nil), p.Connections...)
		}
	}
	return out
}

// migrateProviderConnections promotes a legacy single ApiKey into Connections
// when the slice is empty. Idempotent: a provider that already has connections
// is left alone (restart will not mint Key 2, Key 3, …). Returns true when any
// provider was rewritten.
func migrateProviderConnections(providers []UpstreamProvider) bool {
	changed := false
	for i := range providers {
		if migrateOneProvider(&providers[i]) {
			changed = true
		}
	}
	return changed
}

func migrateOneProvider(p *UpstreamProvider) bool {
	if p == nil {
		return false
	}
	if len(p.Connections) > 0 {
		changed := repairConnectionNames(p)
		if syncLegacyApiKey(p) {
			changed = true
		}
		return changed
	}
	key := strings.TrimSpace(p.ApiKey)
	if key == "" {
		return false
	}
	p.Connections = []UpstreamConnection{{
		ID:      newUUID(),
		Name:    "Key 1",
		ApiKey:  key,
		Enabled: true,
	}}
	return true
}

// syncLegacyApiKey keeps the deprecated ApiKey field equal to the first
// connection's secret so export files and older readers still see a key.
func syncLegacyApiKey(p *UpstreamProvider) bool {
	if p == nil || len(p.Connections) == 0 {
		return false
	}
	first := p.Connections[0].ApiKey
	if p.ApiKey == first {
		return false
	}
	p.ApiKey = first
	return true
}

// ResolvedConnections returns the connections a provider should actually use.
// If Connections is empty but a legacy ApiKey is set, a synthetic Key 1 is
// returned so unit tests and in-memory providers that never went through Load
// still forward with the configured secret.
func ResolvedConnections(p UpstreamProvider) []UpstreamConnection {
	if len(p.Connections) > 0 {
		out := make([]UpstreamConnection, len(p.Connections))
		copy(out, p.Connections)
		return out
	}
	if key := strings.TrimSpace(p.ApiKey); key != "" {
		return []UpstreamConnection{{
			ID:      "",
			Name:    "Key 1",
			ApiKey:  key,
			Enabled: true,
		}}
	}
	return nil
}

// NormalizeConnectionStrategy maps empty/unknown values to a known strategy.
func NormalizeConnectionStrategy(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case ConnectionStrategyPrimary:
		return ConnectionStrategyPrimary
	default:
		return ConnectionStrategyRoundRobin
	}
}

// FirstEnabledAPIKey returns the first enabled connection's key, or the legacy
// ApiKey if no enabled connection exists.
func FirstEnabledAPIKey(p UpstreamProvider) string {
	for _, c := range ResolvedConnections(p) {
		if c.Enabled && strings.TrimSpace(c.ApiKey) != "" {
			return c.ApiKey
		}
	}
	for _, c := range ResolvedConnections(p) {
		if strings.TrimSpace(c.ApiKey) != "" {
			return c.ApiKey
		}
	}
	return strings.TrimSpace(p.ApiKey)
}

// FindConnection returns the connection with the given ID.
func FindConnection(p UpstreamProvider, connectionID string) (UpstreamConnection, bool) {
	id := strings.TrimSpace(connectionID)
	if id == "" {
		return UpstreamConnection{}, false
	}
	for _, c := range p.Connections {
		if c.ID == id {
			return c, true
		}
	}
	return UpstreamConnection{}, false
}

// NextKeyNName returns the smallest unused "Key N" among existing names
// (gap-fill). Names that are not "Key <digits>" are ignored.
func NextKeyNName(existing []string) string {
	used := map[int]bool{}
	for _, name := range existing {
		m := keyNNameRE.FindStringSubmatch(strings.TrimSpace(name))
		if len(m) != 2 {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil || n <= 0 {
			continue
		}
		used[n] = true
	}
	for i := 1; ; i++ {
		if !used[i] {
			return "Key " + strconv.Itoa(i)
		}
	}
}

// ConnectionNames returns the names currently stored on a provider.
func ConnectionNames(p UpstreamProvider) []string {
	names := make([]string, 0, len(p.Connections))
	for _, c := range p.Connections {
		names = append(names, c.Name)
	}
	return names
}

// HasConnectionAPIKey reports whether any connection (or the legacy field)
// already stores this exact key. Comparison is trim-only; keys are case-sensitive.
func HasConnectionAPIKey(p UpstreamProvider, apiKey string) bool {
	want := strings.TrimSpace(apiKey)
	if want == "" {
		return false
	}
	for _, c := range ResolvedConnections(p) {
		if strings.TrimSpace(c.ApiKey) == want {
			return true
		}
	}
	return false
}

// MaskConnectionSecret always hides the middle of a key. Short keys become
// "****" rather than being returned in full (unlike MaskApiKey).
func MaskConnectionSecret(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 10 {
		return "****"
	}
	return key[:6] + "****" + key[len(key)-4:]
}

// IsMaskedSecret reports whether a client-supplied value looks like a mask
// placeholder rather than a real key.
func IsMaskedSecret(s string) bool {
	return strings.Contains(s, "****")
}

// AddUpstreamConnection appends a connection to a provider and persists.
func AddUpstreamConnection(providerID string, conn UpstreamConnection) (UpstreamConnection, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	p, idx, err := findProviderLocked(providerID)
	if err != nil {
		return UpstreamConnection{}, err
	}
	if len(p.Connections) >= maxConnectionsPerProvider {
		return UpstreamConnection{}, ErrTooManyConnections
	}
	key := strings.TrimSpace(conn.ApiKey)
	if key == "" {
		return UpstreamConnection{}, ErrEmptyConnectionKey
	}
	if HasConnectionAPIKey(p, key) {
		return UpstreamConnection{}, ErrDuplicateConnectionKey
	}
	if strings.TrimSpace(conn.ID) == "" {
		conn.ID = newUUID()
	}
	conn.Name = sanitizeConnectionName(conn.Name, key)
	if strings.TrimSpace(conn.Name) == "" {
		conn.Name = NextKeyNName(ConnectionNames(p))
	}
	conn.ApiKey = key
	p.Connections = append(p.Connections, conn)
	syncLegacyApiKey(&p)
	cfg.Upstreams[idx] = p
	if err := saveLocked(); err != nil {
		return UpstreamConnection{}, err
	}
	return conn, nil
}

// AddUpstreamConnections appends many connections in one persist. Duplicates
// against the current provider or earlier items in the batch are skipped.
func AddUpstreamConnections(providerID string, conns []UpstreamConnection) ([]UpstreamConnection, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	p, idx, err := findProviderLocked(providerID)
	if err != nil {
		return nil, err
	}
	added := make([]UpstreamConnection, 0, len(conns))
	seen := map[string]bool{}
	for _, c := range p.Connections {
		if k := strings.TrimSpace(c.ApiKey); k != "" {
			seen[k] = true
		}
	}
	for _, conn := range conns {
		key := strings.TrimSpace(conn.ApiKey)
		if key == "" || seen[key] {
			continue
		}
		if len(p.Connections)+len(added) >= maxConnectionsPerProvider {
			break
		}
		if strings.TrimSpace(conn.ID) == "" {
			conn.ID = newUUID()
		}
		conn.Name = sanitizeConnectionName(conn.Name, key)
		if strings.TrimSpace(conn.Name) == "" {
			names := ConnectionNames(p)
			for _, a := range added {
				names = append(names, a.Name)
			}
			conn.Name = NextKeyNName(names)
		}
		conn.ApiKey = key
		seen[key] = true
		added = append(added, conn)
	}
	if len(added) == 0 {
		return added, nil
	}
	p.Connections = append(p.Connections, added...)
	syncLegacyApiKey(&p)
	cfg.Upstreams[idx] = p
	if err := saveLocked(); err != nil {
		return nil, err
	}
	return added, nil
}

// UpstreamConnectionPatch is a partial update. Nil fields are left unchanged.
type UpstreamConnectionPatch struct {
	Name    *string
	ApiKey  *string
	Enabled *bool
	Weight  *int
}

// UpdateUpstreamConnection patches one connection. An empty or masked ApiKey
// keeps the stored secret.
func UpdateUpstreamConnection(providerID, connectionID string, patch UpstreamConnectionPatch) (UpstreamConnection, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	p, pidx, err := findProviderLocked(providerID)
	if err != nil {
		return UpstreamConnection{}, err
	}
	cidx := -1
	for i, c := range p.Connections {
		if c.ID == connectionID {
			cidx = i
			break
		}
	}
	if cidx < 0 {
		return UpstreamConnection{}, ErrUpstreamConnectionNotFound
	}
	cur := p.Connections[cidx]
	if patch.Name != nil {
		// The incoming key wins for the secret-shape check: renaming and rotating
		// in the same PATCH must not let the new secret through as a label.
		against := cur.ApiKey
		if patch.ApiKey != nil {
			if k := strings.TrimSpace(*patch.ApiKey); k != "" && !IsMaskedSecret(k) {
				against = k
			}
		}
		if name := strings.TrimSpace(sanitizeConnectionName(*patch.Name, against)); name != "" {
			cur.Name = name
		}
	}
	if patch.ApiKey != nil {
		if key := strings.TrimSpace(*patch.ApiKey); key != "" && !IsMaskedSecret(key) {
			if key != cur.ApiKey && HasConnectionAPIKey(p, key) {
				return UpstreamConnection{}, ErrDuplicateConnectionKey
			}
			cur.ApiKey = key
		}
	}
	if patch.Enabled != nil {
		cur.Enabled = *patch.Enabled
	}
	if patch.Weight != nil {
		cur.Weight = *patch.Weight
	}
	p.Connections[cidx] = cur
	syncLegacyApiKey(&p)
	cfg.Upstreams[pidx] = p
	if err := saveLocked(); err != nil {
		return UpstreamConnection{}, err
	}
	return cur, nil
}

// DeleteUpstreamConnection removes one connection and persists.
func DeleteUpstreamConnection(providerID, connectionID string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	p, pidx, err := findProviderLocked(providerID)
	if err != nil {
		return err
	}
	kept := p.Connections[:0]
	found := false
	for _, c := range p.Connections {
		if c.ID == connectionID {
			found = true
			continue
		}
		kept = append(kept, c)
	}
	if !found {
		return ErrUpstreamConnectionNotFound
	}
	p.Connections = append([]UpstreamConnection(nil), kept...)
	syncLegacyApiKey(&p)
	if len(p.Connections) == 0 {
		p.ApiKey = ""
	}
	cfg.Upstreams[pidx] = p
	return saveLocked()
}

// DeleteUpstreamConnections removes every connection whose ID appears in ids
// and persists once. Clearing a selection from the admin key list would
// otherwise be N requests, each taking the config lock and rewriting the file
// while forwards are in flight. Unknown IDs are skipped -- the list the
// operator selected from can be seconds stale -- but a call that matches
// nothing returns ErrUpstreamConnectionNotFound so the UI cannot report a
// successful no-op.
func DeleteUpstreamConnections(providerID string, ids []string) (int, error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	p, pidx, err := findProviderLocked(providerID)
	if err != nil {
		return 0, err
	}
	drop := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			drop[id] = true
		}
	}
	if len(drop) == 0 {
		return 0, ErrUpstreamConnectionNotFound
	}
	// A fresh slice rather than p.Connections[:0]: the backing array is shared
	// with the provider snapshot a forward may be ranging over right now.
	kept := make([]UpstreamConnection, 0, len(p.Connections))
	removed := 0
	for _, c := range p.Connections {
		if drop[c.ID] {
			removed++
			continue
		}
		kept = append(kept, c)
	}
	if removed == 0 {
		return 0, ErrUpstreamConnectionNotFound
	}
	p.Connections = kept
	syncLegacyApiKey(&p)
	if len(p.Connections) == 0 {
		p.ApiKey = ""
	}
	cfg.Upstreams[pidx] = p
	if err := saveLocked(); err != nil {
		return 0, err
	}
	return removed, nil
}

// SetProviderConnectionStrategy updates the load-balancing strategy and persists.
func SetProviderConnectionStrategy(providerID, strategy string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	p, idx, err := findProviderLocked(providerID)
	if err != nil {
		return err
	}
	p.ConnectionStrategy = NormalizeConnectionStrategy(strategy)
	cfg.Upstreams[idx] = p
	return saveLocked()
}

func findProviderLocked(providerID string) (UpstreamProvider, int, error) {
	if cfg == nil {
		return UpstreamProvider{}, -1, ErrUpstreamProviderNotFound
	}
	id := strings.TrimSpace(providerID)
	for i, p := range cfg.Upstreams {
		if p.ID == id {
			return p, i, nil
		}
	}
	return UpstreamProvider{}, -1, ErrUpstreamProviderNotFound
}

// looksLikeSecretName reports whether a connection label is actually a
// credential. Operators paste a whole line into the name box, and bulk imports
// of key-only columns have produced names identical to the key — which then
// renders in the admin UI and prints into [Forward] logs in cleartext.
// The key argument is the connection's own secret (may be empty after rotation).
func looksLikeSecretName(name, key string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	if key = strings.TrimSpace(key); key != "" {
		if name == key || strings.Contains(name, key) {
			return true
		}
	}
	// A rotated key leaves the old secret behind as the label, so also reject
	// anything that simply looks like a token on its own.
	return hasKnownKeyPrefix(name) && len(name) >= 20 && !strings.ContainsAny(name, " \t|,;")
}

// SafeConnectionLabel is the only name that may be shown or logged. Stored
// names are repaired on load, but read paths mask defensively so a config
// written by an older build cannot leak a secret through the UI or the log.
func SafeConnectionLabel(c UpstreamConnection) string {
	if looksLikeSecretName(c.Name, c.ApiKey) {
		return MaskConnectionSecret(strings.TrimSpace(c.Name))
	}
	return c.Name
}

// sanitizeConnectionName blanks a secret-shaped label so the caller's
// "Key N" fallback names the connection instead.
func sanitizeConnectionName(name, key string) string {
	if looksLikeSecretName(name, key) {
		return ""
	}
	return name
}

// repairConnectionNames rewrites labels that hold a raw credential. Runs on load
// so a config written before the guard existed stops leaking the secret into the
// admin list and the forward log on the next start. Names are replaced with the
// next free "Key N" rather than a mask, so the row stays referable.
func repairConnectionNames(p *UpstreamProvider) bool {
	changed := false
	for i := range p.Connections {
		if !looksLikeSecretName(p.Connections[i].Name, p.Connections[i].ApiKey) {
			continue
		}
		p.Connections[i].Name = NextKeyNName(ConnectionNames(*p))
		changed = true
	}
	return changed
}
