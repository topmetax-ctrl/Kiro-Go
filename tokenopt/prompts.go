// Package tokenopt is Kiro-Go's Token Optimization integration layer.
//
// It owns the stable internal contracts (settings resolution, request
// transformation, health, metrics) and treats every external engine as an
// optional, replaceable adapter. It never reimplements an engine's algorithm:
//
//   - Headroom is called over its HTTP API (POST /v1/compress).
//   - RTK has no gateway-level compress API, so it is verified and described,
//     never emulated.
//   - Caveman and Ponytail are applied by injecting the projects' own official
//     prompt text, vendored byte-for-byte under assets/ with provenance.
//
// Every failure path degrades to "send the original request unchanged".
package tokenopt

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"kiro-go/config"
)

//go:embed assets
var assetFS embed.FS

// Asset is the provenance record of one vendored upstream file.
type Asset struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Repo         string `json:"repo"`
	Tag          string `json:"tag"`
	Commit       string `json:"commit"`
	UpstreamPath string `json:"upstreamPath"`
	File         string `json:"file"`
	Bytes        int    `json:"bytes"`
	SHA256       string `json:"sha256"`
	License      string `json:"license"`
	LicenseFile  string `json:"licenseFile"`
	NoticeFile   string `json:"noticeFile,omitempty"`
}

var (
	assetsOnce sync.Once
	assetList  []Asset
	assetBody  map[string]string // id -> text, only for assets that passed verification
	assetErr   error
)

// loadAssets reads the manifest and verifies every file against its pinned
// sha256. A mismatch can only come from a bad build or an edited vendored file;
// the affected assets are withheld rather than injected unverified.
func loadAssets() {
	assetsOnce.Do(func() {
		assetBody = map[string]string{}
		raw, err := assetFS.ReadFile("assets/manifest.json")
		if err != nil {
			assetErr = fmt.Errorf("asset manifest unreadable: %w", err)
			return
		}
		var m struct {
			Assets []Asset `json:"assets"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			assetErr = fmt.Errorf("asset manifest invalid: %w", err)
			return
		}
		assetList = m.Assets
		for _, a := range m.Assets {
			data, err := assetFS.ReadFile("assets/" + a.File)
			if err != nil {
				assetErr = fmt.Errorf("asset %s unreadable: %w", a.ID, err)
				continue
			}
			sum := sha256.Sum256(data)
			if hex.EncodeToString(sum[:]) != a.SHA256 || len(data) != a.Bytes {
				assetErr = fmt.Errorf("asset %s does not match its pinned sha256", a.ID)
				continue
			}
			assetBody[a.ID] = string(data)
		}
	})
}

// Assets returns the provenance of every vendored asset and the first
// verification error, if any.
func Assets() ([]Asset, error) {
	loadAssets()
	return append([]Asset(nil), assetList...), assetErr
}

// LicenseText returns a vendored license or notice file (for display and for
// the redistribution obligations of Apache-2.0 / MIT).
func LicenseText(file string) (string, bool) {
	loadAssets()
	data, err := assetFS.ReadFile("assets/" + file)
	if err != nil {
		return "", false
	}
	return string(data), true
}

func body(id string) string {
	loadAssets()
	return assetBody[id]
}

var frontmatterRE = regexp.MustCompile(`(?s)\A---.*?---\s*`)

// stripFrontmatter drops a leading YAML block, as Ponytail's own instruction
// builder does (hooks/ponytail-instructions.js: replace(/^---[\s\S]*?---\s*/, ”)).
func stripFrontmatter(s string) string { return frontmatterRE.ReplaceAllString(s, "") }

// CavemanText returns the official Caveman text for a style, or "" when off or
// unknown. "caveman" is the project's always-on activation rule (the text its
// installer writes into agent rule files); "ultracave" is the ultracave skill
// body. The Classical-Chinese megacave variant is deliberately not offered: it
// switches the reply language, which a gateway has no business doing silently.
func CavemanText(style string) string {
	switch style {
	case config.TokenOptCaveman:
		return body("caveman.caveman")
	case config.TokenOptUltracave:
		return stripFrontmatter(body("caveman.ultracave"))
	}
	return ""
}

var (
	ponytailTableRE   = regexp.MustCompile(`^\|\s*\*\*(.+?)\*\*\s*\|`)
	ponytailExampleRE = regexp.MustCompile(`^-\s*([^:]+):\s*"`)
	newlineRE         = regexp.MustCompile(`\r?\n`)
)

// ponytailRuntimeMode mirrors normalizeMode() in Ponytail's hooks/ponytail-config.js.
func ponytailRuntimeMode(s string) string {
	switch v := strings.ToLower(strings.TrimSpace(s)); v {
	case "off", "lite", "full", "ultra":
		return v
	}
	return ""
}

// PonytailText returns Ponytail's official instruction text for a level, or ""
// when off or unknown. It is a faithful port of getPonytailInstructions() from
// the project's hooks/ponytail-instructions.js: the header line, then SKILL.md
// without its frontmatter and without the intensity rows / worked examples that
// belong to other levels. The rules themselves come only from the vendored file.
func PonytailText(level string) string {
	mode := ponytailRuntimeMode(level)
	if mode == "" || mode == config.TokenOptPonytailOff {
		return ""
	}
	skill := body("ponytail.skill")
	if skill == "" {
		return ""
	}
	lines := newlineRE.Split(stripFrontmatter(skill), -1)
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if m := ponytailTableRE.FindStringSubmatch(line); m != nil {
			if lm := ponytailRuntimeMode(strings.TrimSpace(m[1])); lm != "" && lm != mode {
				continue
			}
		}
		if m := ponytailExampleRE.FindStringSubmatch(line); m != nil {
			if lm := ponytailRuntimeMode(strings.TrimSpace(m[1])); lm != "" && lm != mode {
				continue
			}
		}
		kept = append(kept, line)
	}
	return "PONYTAIL MODE ACTIVE — level: " + mode + "\n\n" + strings.Join(kept, "\n")
}

// Sentinels are literal strings the upstream projects' own installers put into
// an agent's prompt. When one is already present the client has the tool
// installed, and injecting it again would double the instruction and its cost.
var (
	cavemanSentinels  = []string{"Respond terse like smart caveman"}
	ponytailSentinels = []string{"PONYTAIL MODE ACTIVE", "You are a lazy senior developer. The best code is the code never written."}
)

func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// Behavior is the deterministic block of official prompt text selected by the
// Caveman and Ponytail settings.
type Behavior struct {
	// Text is the full block, "" when nothing is selected. Ponytail (what to
	// build) comes before Caveman (how to speak), joined by one blank line.
	Text string
	// Caveman and Ponytail are the levels that contributed to Text.
	Caveman  string
	Ponytail string
	// Hash identifies Text in logs without printing it.
	Hash string

	cavemanText  string
	ponytailText string
}

// Empty reports whether there is nothing to inject.
func (b Behavior) Empty() bool { return b.Text == "" }

// ComposeBehavior selects the official text for the given levels. The result is
// a pure function of its inputs, so the same settings always produce the same
// bytes and a stable prompt prefix.
func ComposeBehavior(caveman, ponytail string) Behavior {
	return compose(caveman, CavemanText(caveman), ponytail, PonytailText(ponytail))
}

func compose(caveman, cavemanText, ponytail, ponytailText string) Behavior {
	b := Behavior{cavemanText: cavemanText, ponytailText: ponytailText}
	var parts []string
	if ponytailText != "" {
		b.Ponytail = ponytail
		parts = append(parts, strings.TrimRight(ponytailText, " \t\r\n"))
	}
	if cavemanText != "" {
		b.Caveman = caveman
		parts = append(parts, strings.TrimRight(cavemanText, " \t\r\n"))
	}
	if len(parts) == 0 {
		return b
	}
	b.Text = strings.Join(parts, "\n\n")
	sum := sha256.Sum256([]byte(b.Text))
	b.Hash = hex.EncodeToString(sum[:])[:12]
	return b
}

// WithoutPresent returns the behavior minus any part whose official sentinel
// already appears in system, so a client that has Caveman or Ponytail installed
// is never given a second copy.
func (b Behavior) WithoutPresent(system string) Behavior {
	caveman, cavemanText := b.Caveman, b.cavemanText
	ponytail, ponytailText := b.Ponytail, b.ponytailText
	if containsAny(system, cavemanSentinels) {
		caveman, cavemanText = "", ""
	}
	if containsAny(system, ponytailSentinels) {
		ponytail, ponytailText = "", ""
	}
	return compose(caveman, cavemanText, ponytail, ponytailText)
}
