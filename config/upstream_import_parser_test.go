package config

import "testing"

func TestParseConnectionImportFormats(t *testing.T) {
	text := "sk-only-key-abcdefghijklmnopqrstuvwxyz\n" +
		"alice | sk-name-key-abcdefghijklmnopqrstuv\n" +
		"user@example.com | password123 | sk-email-key-abcdefghijklmnop\n" +
		"  bob  |  secret  |  sk-spaces-key-abcdefghijklmnop  \n" +
		"carol\tsk-tab-key-abcdefghijklmnopqrstuvwx\n"

	prev := ParseConnectionImport(text, "auto", NamingFirstNonKey, nil, nil, nil)
	if prev.Ready != 5 || prev.Ambiguous != 0 || prev.Invalid != 0 {
		t.Fatalf("counts ready=%d amb=%d inv=%d lines=%d", prev.Ready, prev.Ambiguous, prev.Invalid, len(prev.Lines))
	}

	want := []struct {
		name string
		key  string
	}{
		{"Key 1", "sk-only-key-abcdefghijklmnopqrstuvwxyz"},
		{"alice", "sk-name-key-abcdefghijklmnopqrstuv"},
		{"user@example.com", "sk-email-key-abcdefghijklmnop"},
		{"bob", "sk-spaces-key-abcdefghijklmnop"},
		{"carol", "sk-tab-key-abcdefghijklmnopqrstuvwx"},
	}
	for i, w := range want {
		row := prev.Lines[i]
		if row.Status != ImportStatusReady || row.Name != w.name || row.Key != w.key {
			t.Errorf("line %d: status=%s name=%q key=%q, want ready/%q/%q", i+1, row.Status, row.Name, row.Key, w.name, w.key)
		}
	}
}

func TestParseConnectionImportCRLFAndBOM(t *testing.T) {
	text := "\ufeffalice | sk-crlf-key-abcdefghijklmnopqrst\r\nbob | sk-lf-key-abcdefghijklmnopqrstuv"
	prev := ParseConnectionImport(text, "auto", NamingFirstNonKey, nil, nil, nil)
	if prev.Ready != 2 {
		t.Fatalf("ready = %d, want 2 (%+v)", prev.Ready, prev.Lines)
	}
	if prev.Lines[0].Name != "alice" || prev.Lines[1].Name != "bob" {
		t.Fatalf("names = %q, %q", prev.Lines[0].Name, prev.Lines[1].Name)
	}
}

func TestParseConnectionImportDoesNotJoinPassword(t *testing.T) {
	text := "user@example.com | hunter2 | sk-join-key-abcdefghijklmnop"
	prev := ParseConnectionImport(text, "auto", NamingFirstNonKey, nil, nil, nil)
	if prev.Ready != 1 {
		t.Fatalf("ready = %d", prev.Ready)
	}
	if prev.Lines[0].Name != "user@example.com" {
		t.Fatalf("name = %q, password must not be joined", prev.Lines[0].Name)
	}
	joined := ParseConnectionImport(text, "auto", NamingJoinNonKey, nil, nil, nil)
	if joined.Lines[0].Name != "user@example.com | hunter2" {
		t.Fatalf("join name = %q", joined.Lines[0].Name)
	}
}

func TestParseConnectionImportAmbiguousWithoutPrefix(t *testing.T) {
	text := "abc123xxxxxxxxxxx | xyz987xxxxxxxxxxx"
	prev := ParseConnectionImport(text, "auto", NamingFirstNonKey, nil, nil, nil)
	if prev.Ambiguous != 1 || prev.Ready != 0 {
		t.Fatalf("want ambiguous, got ready=%d amb=%d (%+v)", prev.Ready, prev.Ambiguous, prev.Lines)
	}
	resolved := ParseConnectionImport(text, "auto", NamingFirstNonKey, nil, nil, []ConnectionImportResolution{{Line: 1, Column: 1}})
	if resolved.Ready != 1 || resolved.Lines[0].Key != "xyz987xxxxxxxxxxx" {
		t.Fatalf("resolution failed: %+v", resolved.Lines)
	}
}

func TestParseConnectionImportDedupesExistingAndBatch(t *testing.T) {
	text := "sk-dup-key-abcdefghijklmnopqrstuv\nsk-dup-key-abcdefghijklmnopqrstuv\nsk-new-key-abcdefghijklmnopqrstuv"
	prev := ParseConnectionImport(text, "auto", NamingFirstNonKey, []string{"sk-dup-key-abcdefghijklmnopqrstuv"}, nil, nil)
	if prev.Ready != 1 || prev.Duplicate != 2 {
		t.Fatalf("ready=%d dup=%d, want 1/2", prev.Ready, prev.Duplicate)
	}
}

func TestParseConnectionImportGapFillKeyN(t *testing.T) {
	text := "sk-a-key-abcdefghijklmnopqrstuvwxyz\nsk-b-key-abcdefghijklmnopqrstuvwxyz"
	prev := ParseConnectionImport(text, "auto", NamingKeyN, nil, []string{"Key 1", "Key 3"}, nil)
	if prev.Ready != 2 {
		t.Fatalf("ready = %d", prev.Ready)
	}
	if prev.Lines[0].Name != "Key 2" || prev.Lines[1].Name != "Key 4" {
		t.Fatalf("names = %q, %q, want Key 2 then Key 4", prev.Lines[0].Name, prev.Lines[1].Name)
	}
}

func TestParseConnectionImportNonSkPrefix(t *testing.T) {
	text := "gsk_abcdefghijklmnopqrstuvwxyz012345\nhf_abcdefghijklmnopqrstuvwxyz0123456"
	prev := ParseConnectionImport(text, "auto", NamingFirstNonKey, nil, nil, nil)
	if prev.Ready != 2 {
		t.Fatalf("ready = %d, want 2 (gsk_/hf_ should score as keys)", prev.Ready)
	}
}
