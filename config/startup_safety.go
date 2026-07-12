package config

import (
	"fmt"
	"os"
	"strings"
)

// allowInsecurePublicBindEnv, when set to "true"/"1", downgrades the public-bind
// startup hard-fail to a loud warning. It is never enabled by default and must be
// set explicitly by an operator who understands the exposure.
const allowInsecurePublicBindEnv = "ALLOW_INSECURE_PUBLIC_BIND"

// StartupSafetyInput is the resolved deployment posture the safety gate evaluates.
// It is populated from the live config but passed explicitly so the decision logic
// is pure and unit-testable without global state.
type StartupSafetyInput struct {
	Host            string
	DefaultPassword bool // admin password is still "changeme"
	AuthDisabled    bool // API-key auth is not enforced (anyone can call the proxy)
	AllowInsecure   bool // operator override (ALLOW_INSECURE_PUBLIC_BIND)
}

// isLoopbackBind reports whether host binds only to the local machine. An empty
// host resolves to loopback (see GetHost). Wildcard binds (0.0.0.0 / ::) and any
// concrete non-loopback address are treated as public.
func isLoopbackBind(host string) bool {
	h := strings.TrimSpace(strings.ToLower(host))
	switch h {
	case "", "127.0.0.1", "::1", "localhost":
		return true
	default:
		return false
	}
}

// EvaluateStartupSafety returns a fatal error when the deployment is exposed to a
// non-loopback address with a weak posture (default admin password, or no API-key
// auth). It returns a non-nil warning string (with nil error) when the operator has
// explicitly opted into an insecure public bind, so the caller can log it loudly.
//
// The gate never blocks a loopback-only bind, and never blocks a public bind that
// has both a changed password and enforced auth. It never includes secret values in
// its messages.
func EvaluateStartupSafety(in StartupSafetyInput) (warning string, err error) {
	if isLoopbackBind(in.Host) {
		return "", nil
	}

	// Public bind from here on.
	var problems []string
	if in.DefaultPassword {
		problems = append(problems, `admin password is still the default "changeme"`)
	}
	if in.AuthDisabled {
		problems = append(problems, "API-key authentication is disabled (anyone can use the proxy)")
	}

	if len(problems) == 0 {
		return "", nil
	}

	detail := strings.Join(problems, "; ")
	if in.AllowInsecure {
		return fmt.Sprintf(
			"insecure public bind permitted by %s: server is bound to %q but %s. This is dangerous; set a strong admin password and enable API-key auth.",
			allowInsecurePublicBindEnv, in.Host, detail,
		), nil
	}
	return "", fmt.Errorf(
		"refusing to start: server is bound to public address %q but %s. "+
			"Bind to 127.0.0.1, set a strong admin password (ADMIN_PASSWORD), and enable API-key auth. "+
			"To override at your own risk set %s=true.",
		in.Host, detail, allowInsecurePublicBindEnv,
	)
}

// CheckStartupSafety resolves the live deployment posture and applies the gate. It
// returns a fatal error the caller should exit on, or a warning string to log.
func CheckStartupSafety() (warning string, err error) {
	in := StartupSafetyInput{
		Host:            GetHost(),
		DefaultPassword: GetPassword() == "changeme",
		AuthDisabled:    !IsApiKeyRequired(),
		AllowInsecure:   isTruthyEnv(os.Getenv(allowInsecurePublicBindEnv)),
	}
	return EvaluateStartupSafety(in)
}

func isTruthyEnv(v string) bool {
	switch strings.TrimSpace(strings.ToLower(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
