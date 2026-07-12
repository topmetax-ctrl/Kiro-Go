package config

import (
	"strings"
	"testing"
)

func TestEvaluateStartupSafety(t *testing.T) {
	cases := []struct {
		name     string
		in       StartupSafetyInput
		wantErr  bool
		wantWarn bool
	}{
		{
			name: "loopback with default password and no auth is fine",
			in:   StartupSafetyInput{Host: "127.0.0.1", DefaultPassword: true, AuthDisabled: true},
		},
		{
			name: "empty host resolves to loopback and is fine",
			in:   StartupSafetyInput{Host: "", DefaultPassword: true, AuthDisabled: true},
		},
		{
			name: "localhost is loopback",
			in:   StartupSafetyInput{Host: "localhost", DefaultPassword: true, AuthDisabled: true},
		},
		{
			name: "ipv6 loopback is fine",
			in:   StartupSafetyInput{Host: "::1", DefaultPassword: true, AuthDisabled: true},
		},
		{
			name:    "public bind with default password hard-fails",
			in:      StartupSafetyInput{Host: "0.0.0.0", DefaultPassword: true, AuthDisabled: false},
			wantErr: true,
		},
		{
			name:    "public bind with auth disabled hard-fails",
			in:      StartupSafetyInput{Host: "0.0.0.0", DefaultPassword: false, AuthDisabled: true},
			wantErr: true,
		},
		{
			name:    "concrete public IP with weak posture hard-fails",
			in:      StartupSafetyInput{Host: "10.0.0.5", DefaultPassword: true, AuthDisabled: true},
			wantErr: true,
		},
		{
			name:    "ipv6 wildcard is public",
			in:      StartupSafetyInput{Host: "::", DefaultPassword: true, AuthDisabled: false},
			wantErr: true,
		},
		{
			name: "public bind with strong password and auth on is fine",
			in:   StartupSafetyInput{Host: "0.0.0.0", DefaultPassword: false, AuthDisabled: false},
		},
		{
			name:     "override downgrades hard-fail to warning",
			in:       StartupSafetyInput{Host: "0.0.0.0", DefaultPassword: true, AuthDisabled: true, AllowInsecure: true},
			wantWarn: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			warn, err := EvaluateStartupSafety(tc.in)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil (warn=%q)", warn)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantWarn && warn == "" {
				t.Fatalf("expected warning, got empty")
			}
			if !tc.wantWarn && !tc.wantErr && warn != "" {
				t.Fatalf("expected no warning, got %q", warn)
			}
			// Never leak the actual password value; the literal "changeme" may
			// appear as descriptive text but no other secret should.
			if err != nil && strings.Contains(err.Error(), "password is") &&
				!strings.Contains(err.Error(), "default") {
				t.Fatalf("error text should describe, not leak: %v", err)
			}
		})
	}
}

func TestIsTruthyEnv(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on", " true "} {
		if !isTruthyEnv(v) {
			t.Errorf("expected %q truthy", v)
		}
	}
	for _, v := range []string{"", "0", "false", "no", "off", "maybe"} {
		if isTruthyEnv(v) {
			t.Errorf("expected %q falsy", v)
		}
	}
}
