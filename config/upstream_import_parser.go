package config

import (
	"net/mail"
	"net/url"
	"strings"
	"unicode"
)

// Connection naming strategies for bulk import.
const (
	NamingFirstNonKey = "first_non_key"
	NamingJoinNonKey  = "join_non_key"
	NamingKeyN        = "key_n"
)

// Import line statuses returned by the preview/commit parser.
const (
	ImportStatusReady         = "ready"
	ImportStatusDuplicate     = "duplicate"
	ImportStatusAmbiguous     = "ambiguous"
	ImportStatusInvalid       = "invalid"
	ImportConfidenceHigh      = "high"
	ImportConfidenceMedium    = "medium"
	ImportConfidenceLow       = "low"
	ImportConfidenceAmbiguous = "ambiguous"
)

// ImportCandidate is one scored field that might be the API key.
type ImportCandidate struct {
	Index  int    `json:"index"`
	Masked string `json:"masked"`
	Score  int    `json:"score"`
}

// ParsedImportLine is one input row after detection.
type ParsedImportLine struct {
	LineNum    int               `json:"line"`
	Raw        string            `json:"-"`
	Fields     []string          `json:"-"`
	KeyIndex   int               `json:"keyIndex"`
	Key        string            `json:"-"`
	KeyMasked  string            `json:"keyMasked"`
	Name       string            `json:"name"`
	Confidence string            `json:"confidence"`
	Status     string            `json:"status"`
	Candidates []ImportCandidate `json:"candidates,omitempty"`
}

// ConnectionImportPreview is the Analyze result for a paste.
type ConnectionImportPreview struct {
	Lines     []ParsedImportLine `json:"lines"`
	Ready     int                `json:"ready"`
	Duplicate int                `json:"duplicate"`
	Ambiguous int                `json:"ambiguous"`
	Invalid   int                `json:"invalid"`
}

// ConnectionImportResolution lets the operator pick a key column for an
// ambiguous line (1-based line number → 0-based field index).
type ConnectionImportResolution struct {
	Line   int `json:"line"`
	Column int `json:"column"`
}

func NormalizeConnectionNaming(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case NamingJoinNonKey:
		return NamingJoinNonKey
	case NamingKeyN:
		return NamingKeyN
	default:
		return NamingFirstNonKey
	}
}

// NormalizeImportText strips a BOM and normalizes newlines.
func NormalizeImportText(s string) string {
	s = strings.TrimPrefix(s, "\ufeff")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return s
}

func unquoteField(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return strings.TrimSpace(s[1 : len(s)-1])
		}
	}
	return s
}

func detectDelimiter(line string) string {
	for _, d := range []string{"|", "\t", ";", ","} {
		if strings.Contains(line, d) {
			return d
		}
	}
	return ""
}

func splitImportFields(line, delim string) []string {
	if delim == "" {
		if f := unquoteField(line); f != "" {
			return []string{f}
		}
		return nil
	}
	parts := strings.Split(line, delim)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if f := unquoteField(p); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func isEmailToken(s string) bool {
	if !strings.Contains(s, "@") {
		return false
	}
	_, err := mail.ParseAddress(s)
	return err == nil
}

func isURLToken(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.IsAbs() && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https")
}

func isKeyCharset(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func hasKnownKeyPrefix(s string) bool {
	switch {
	case strings.HasPrefix(s, "sk-proj-"),
		strings.HasPrefix(s, "sk-"),
		strings.HasPrefix(s, "gsk_"),
		strings.HasPrefix(s, "hf_"),
		strings.HasPrefix(s, "AIza"),
		strings.HasPrefix(strings.ToLower(s), "bearer "):
		return true
	default:
		return false
	}
}

func isObviousShortPassword(s string) bool {
	if hasKnownKeyPrefix(s) || isEmailToken(s) || isURLToken(s) {
		return false
	}
	n := len(s)
	return n >= 4 && n < 16
}

func scoreImportToken(tok string, isLast bool) int {
	score := 0
	if hasKnownKeyPrefix(tok) {
		score += 100
	}
	if len(tok) >= 20 {
		score += 20
	}
	if !strings.ContainsAny(tok, " \t") {
		score += 10
	}
	if isKeyCharset(tok) {
		score += 10
	}
	if isLast {
		score += 5
	}
	if isEmailToken(tok) {
		score -= 100
	}
	if isURLToken(tok) {
		score -= 100
	}
	if isObviousShortPassword(tok) {
		score -= 30
	}
	return score
}

func nameFromFields(fields []string, keyIndex int, naming string, fallback string) string {
	switch naming {
	case NamingKeyN:
		return fallback
	case NamingJoinNonKey:
		parts := make([]string, 0, len(fields))
		for i, f := range fields {
			if i == keyIndex {
				continue
			}
			parts = append(parts, f)
		}
		if len(parts) == 0 {
			return fallback
		}
		return strings.Join(parts, " | ")
	default:
		for i, f := range fields {
			if i == keyIndex {
				continue
			}
			return f
		}
		return fallback
	}
}

// ParseConnectionImport analyzes pasted credentials. existingKeys are the
// secrets already stored on the provider (used for duplicate marking).
// existingNames drive gap-fill for auto "Key N" labels.
// resolutions override the detected key column for specific 1-based line numbers.
func ParseConnectionImport(text, delimiter, naming string, existingKeys, existingNames []string, resolutions []ConnectionImportResolution) ConnectionImportPreview {
	text = NormalizeImportText(text)
	naming = NormalizeConnectionNaming(naming)
	existing := map[string]bool{}
	for _, k := range existingKeys {
		if k = strings.TrimSpace(k); k != "" {
			existing[k] = true
		}
	}
	usedNames := append([]string(nil), existingNames...)
	resolveByLine := map[int]int{}
	for _, r := range resolutions {
		resolveByLine[r.Line] = r.Column
	}

	preview := ConnectionImportPreview{Lines: []ParsedImportLine{}}
	batchSeen := map[string]bool{}

	rawLines := strings.Split(text, "\n")
	lineNum := 0
	for _, raw := range rawLines {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		lineNum++
		delim := delimiter
		if delim == "" || strings.EqualFold(delim, "auto") {
			delim = detectDelimiter(trimmed)
		}
		fields := splitImportFields(trimmed, delim)
		row := ParsedImportLine{
			LineNum:    lineNum,
			Raw:        trimmed,
			Fields:     fields,
			KeyIndex:   -1,
			Confidence: ImportConfidenceLow,
			Status:     ImportStatusInvalid,
		}
		if len(fields) == 0 {
			preview.Invalid++
			preview.Lines = append(preview.Lines, row)
			continue
		}

		type scored struct {
			idx   int
			score int
		}
		scores := make([]scored, len(fields))
		for i, f := range fields {
			scores[i] = scored{idx: i, score: scoreImportToken(f, i == len(fields)-1)}
			row.Candidates = append(row.Candidates, ImportCandidate{
				Index:  i,
				Masked: MaskConnectionSecret(f),
				Score:  scores[i].score,
			})
		}
		best, second := scores[0], scored{idx: -1, score: -1 << 30}
		for i := 1; i < len(scores); i++ {
			if scores[i].score > best.score {
				second = best
				best = scores[i]
			} else if scores[i].score > second.score {
				second = scores[i]
			}
		}

		chosen := -1
		if col, ok := resolveByLine[lineNum]; ok && col >= 0 && col < len(fields) {
			chosen = col
			row.Confidence = ImportConfidenceHigh
		} else if best.score <= 0 && len(fields) > 1 {
			row.Confidence = ImportConfidenceAmbiguous
			row.Status = ImportStatusAmbiguous
			preview.Ambiguous++
			preview.Lines = append(preview.Lines, row)
			continue
		} else if len(fields) > 1 && second.idx >= 0 && best.score-second.score < 20 {
			row.Confidence = ImportConfidenceAmbiguous
			row.Status = ImportStatusAmbiguous
			preview.Ambiguous++
			preview.Lines = append(preview.Lines, row)
			continue
		} else {
			chosen = best.idx
			gap := best.score - second.score
			if second.idx < 0 {
				gap = best.score
			}
			switch {
			case best.score >= 100 || gap >= 20:
				row.Confidence = ImportConfidenceHigh
			case gap >= 10:
				row.Confidence = ImportConfidenceMedium
			default:
				row.Confidence = ImportConfidenceLow
			}
		}

		key := fields[chosen]
		if strings.TrimSpace(key) == "" {
			preview.Invalid++
			preview.Lines = append(preview.Lines, row)
			continue
		}
		row.KeyIndex = chosen
		row.Key = key
		row.KeyMasked = MaskConnectionSecret(key)
		if existing[key] || batchSeen[key] {
			row.Status = ImportStatusDuplicate
			preview.Duplicate++
			preview.Lines = append(preview.Lines, row)
			continue
		}
		batchSeen[key] = true
		fallback := NextKeyNName(usedNames)
		row.Name = nameFromFields(fields, chosen, naming, fallback)
		if naming == NamingKeyN {
			row.Name = fallback
		}
		usedNames = append(usedNames, row.Name)
		row.Status = ImportStatusReady
		preview.Ready++
		preview.Lines = append(preview.Lines, row)
	}
	return preview
}

// ExistingConnectionKeys collects raw API keys from a provider.
func ExistingConnectionKeys(p UpstreamProvider) []string {
	conns := ResolvedConnections(p)
	out := make([]string, 0, len(conns))
	for _, c := range conns {
		if k := strings.TrimSpace(c.ApiKey); k != "" {
			out = append(out, k)
		}
	}
	return out
}
