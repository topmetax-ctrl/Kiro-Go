package tokenopt

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"kiro-go/config"
)

// Every vendored file must match its pinned hash and ship with its license; a
// silently edited asset would be injected under an upstream project's name.
func TestAssetsAreVerifiedAndLicensed(t *testing.T) {
	assets, err := Assets()
	if err != nil {
		t.Fatalf("asset verification failed: %v", err)
	}
	if len(assets) != 3 {
		t.Fatalf("assets = %d, want 3", len(assets))
	}
	for _, a := range assets {
		if body(a.ID) == "" {
			t.Errorf("%s: verified body is empty", a.ID)
		}
		if a.Tag == "" || len(a.Commit) != 40 || a.License == "" {
			t.Errorf("%s: incomplete provenance %+v", a.ID, a)
		}
		if txt, ok := LicenseText(a.LicenseFile); !ok || len(txt) < 500 {
			t.Errorf("%s: license file %s missing or truncated", a.ID, a.LicenseFile)
		}
		if a.NoticeFile != "" {
			if _, ok := LicenseText(a.NoticeFile); !ok {
				t.Errorf("%s: notice file %s missing", a.ID, a.NoticeFile)
			}
		}
	}
}

// The port of Ponytail's instruction builder must be byte-identical to the
// project's own JavaScript. The golden file holds the output of
// getPonytailInstructions(level) from hooks/ponytail-instructions.js at the
// pinned tag (v5.3.2), produced with:
//
//	node --permission --allow-fs-read=/tmp --allow-fs-read=/private/tmp -e \
//	  'const {getPonytailInstructions:g}=require("<clone>/hooks/ponytail-instructions.js");
//	   const o={};for(const m of ["lite","full","ultra"])o[m]=g(m);
//	   process.stdout.write(JSON.stringify(o))'
//
// (the permission flags make the untrusted upstream script read-only: no
// network, no writes, no child processes). Regenerate it whenever the vendored
// SKILL.md is re-pinned.
func TestPonytailMatchesOfficialBuilder(t *testing.T) {
	raw, err := os.ReadFile("testdata/ponytail-official.json")
	if err != nil {
		t.Fatal(err)
	}
	var official map[string]string
	if err := json.Unmarshal(raw, &official); err != nil {
		t.Fatal(err)
	}
	for _, level := range []string{"lite", "full", "ultra"} {
		want, ok := official[level]
		if !ok {
			t.Fatalf("golden missing level %q", level)
		}
		if got := PonytailText(level); got != want {
			t.Errorf("level %s differs from the official builder:\n--- got ---\n%q\n--- want ---\n%q", level, got, want)
		}
	}
}

// Each level keeps only its own intensity row, and the level named in the
// header is the level asked for.
func TestPonytailLevelsKeepOnlyTheirOwnRow(t *testing.T) {
	rows := map[string]string{"lite": "| **lite** |", "full": "| **full** |", "ultra": "| **ultra** |"}
	for level, own := range rows {
		got := PonytailText(level)
		if !strings.HasPrefix(got, "PONYTAIL MODE ACTIVE — level: "+level+"\n\n") {
			t.Errorf("%s: wrong header: %.60q", level, got)
		}
		if !strings.Contains(got, own) {
			t.Errorf("%s: own row missing", level)
		}
		for other, row := range rows {
			if other != level && strings.Contains(got, row) {
				t.Errorf("%s: leaked the %s row", level, other)
			}
		}
		if strings.Contains(got, "name: ponytail") {
			t.Errorf("%s: frontmatter was not stripped", level)
		}
	}
}

func TestPonytailOffAndUnknownYieldNothing(t *testing.T) {
	for _, level := range []string{"", "off", "OFF", "review", "extreme", "wenyan"} {
		if got := PonytailText(level); got != "" {
			t.Errorf("PonytailText(%q) = %.40q, want empty", level, got)
		}
	}
}

func TestCavemanTextIsVerbatimOfficialAsset(t *testing.T) {
	if got, want := CavemanText(config.TokenOptCaveman), body("caveman.caveman"); got != want || got == "" {
		t.Fatalf("caveman text is not the vendored activation rule verbatim")
	}
	ultra := CavemanText(config.TokenOptUltracave)
	if !strings.HasPrefix(ultra, "# ultracave") {
		t.Fatalf("ultracave must start at the skill body, got %.40q", ultra)
	}
	if strings.Contains(ultra, "disable-model-invocation") {
		t.Fatalf("ultracave frontmatter leaked into the injected text")
	}
	if !strings.HasSuffix(body("caveman.ultracave"), ultra) {
		t.Fatalf("ultracave text must be an unmodified suffix of the vendored skill")
	}
	for _, style := range []string{"", "off", "megacave", "wenyan", "lite"} {
		if got := CavemanText(style); got != "" {
			t.Errorf("CavemanText(%q) = %.30q, want empty (megacave/wenyan are not offered)", style, got)
		}
	}
}

// If upstream ever rewrites its text so the sentinel disappears, dedup would
// silently stop working; fail loudly at vendoring time instead.
func TestOfficialTextCarriesItsOwnDedupSentinel(t *testing.T) {
	for _, style := range []string{config.TokenOptCaveman, config.TokenOptUltracave} {
		if !containsAny(CavemanText(style), cavemanSentinels) {
			t.Errorf("caveman %s no longer contains its dedup sentinel", style)
		}
	}
	for _, level := range []string{"lite", "full", "ultra"} {
		if !containsAny(PonytailText(level), ponytailSentinels) {
			t.Errorf("ponytail %s no longer contains its dedup sentinel", level)
		}
	}
}

func TestComposeBehaviorOrderAndDeterminism(t *testing.T) {
	a := ComposeBehavior("caveman", "full")
	b := ComposeBehavior("caveman", "full")
	if a.Text != b.Text || a.Hash != b.Hash || a.Hash == "" {
		t.Fatalf("compose is not deterministic: %q vs %q", a.Hash, b.Hash)
	}
	pi, ci := strings.Index(a.Text, "PONYTAIL MODE ACTIVE"), strings.Index(a.Text, "Respond terse like smart caveman")
	if pi < 0 || ci < 0 || pi > ci {
		t.Fatalf("ponytail (what to build) must precede caveman (how to speak): ponytail@%d caveman@%d", pi, ci)
	}
	if strings.HasSuffix(a.Text, "\n") {
		t.Fatalf("block must not end with trailing whitespace")
	}
	if a.Caveman != "caveman" || a.Ponytail != "full" {
		t.Fatalf("contributing levels not recorded: %+v", a)
	}

	only := ComposeBehavior("off", "lite")
	if strings.Contains(only.Text, "caveman") || only.Caveman != "" {
		t.Fatalf("caveman off must contribute nothing: %+v", only)
	}
	if c := ComposeBehavior("ultracave", "full"); c.Hash == a.Hash {
		t.Fatalf("different settings must produce a different hash")
	}
	if none := ComposeBehavior("off", "off"); !none.Empty() || none.Hash != "" {
		t.Fatalf("nothing selected must be empty: %+v", none)
	}
}

func TestWithoutPresentDropsOnlyWhatTheClientAlreadyHas(t *testing.T) {
	full := ComposeBehavior("caveman", "full")

	// Client already runs Caveman: keep only Ponytail.
	got := full.WithoutPresent("You are helpful.\n\nRespond terse like smart caveman. All technical substance stay.")
	if got.Caveman != "" || got.Ponytail != "full" || strings.Contains(got.Text, "Respond terse") {
		t.Fatalf("caveman should have been dropped: %+v", got)
	}
	// Client already runs Ponytail: keep only Caveman.
	got = full.WithoutPresent("PONYTAIL MODE ACTIVE — level: lite\n\nbody")
	if got.Ponytail != "" || got.Caveman != "caveman" || strings.Contains(got.Text, "PONYTAIL MODE ACTIVE") {
		t.Fatalf("ponytail should have been dropped: %+v", got)
	}
	// Both present: nothing left to inject.
	got = full.WithoutPresent("PONYTAIL MODE ACTIVE and Respond terse like smart caveman")
	if !got.Empty() {
		t.Fatalf("both present must leave nothing to inject: %+v", got)
	}
	// Neither present: unchanged, byte for byte.
	if got = full.WithoutPresent("You are a helpful assistant."); got.Text != full.Text || got.Hash != full.Hash {
		t.Fatalf("clean system prompt must not change the block")
	}
}
