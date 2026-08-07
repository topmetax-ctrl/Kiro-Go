package injector

import (
	"fmt"
	"os/exec"
	"runtime"
	"time"
)

// AppTarget identifies which application's processes to control.
type AppTarget int

const (
	AppKiroIDE AppTarget = iota
	AppKiroCLI
)

// KillApp terminates the running instances of the target app so it reloads the
// freshly injected credentials on next launch. It is best-effort: a "no process
// found" outcome is not an error (the app simply wasn't running).
//
// Kiro IDE is an Electron app; the Amazon Q / kiro-cli "app" is the q chat
// process. The CLI reads its token on each invocation, so killing it is usually
// unnecessary — we still stop any long-lived `q chat` session for consistency.
func KillApp(target AppTarget) error {
	switch runtime.GOOS {
	case "darwin", "linux":
		return killUnix(target)
	case "windows":
		return killWindows(target)
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
}

func killUnix(target AppTarget) error {
	var patterns []string
	switch target {
	case AppKiroIDE:
		// Match the Kiro Electron app and its helpers without nuking unrelated
		// processes. -f matches the full command line.
		patterns = []string{"-f", "Kiro.app"}
	case AppKiroCLI:
		patterns = []string{"-x", "kiro-cli"}
	}
	cmd := exec.Command("pkill", patterns...)
	// pkill exit code 1 == "no processes matched", which is fine.
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return nil
		}
		return fmt.Errorf("pkill: %w", err)
	}
	time.Sleep(1 * time.Second) // let the OS reap before relaunch
	return nil
}

func killWindows(target AppTarget) error {
	var image string
	switch target {
	case AppKiroIDE:
		image = "Kiro.exe"
	case AppKiroCLI:
		image = "kiro-cli.exe"
	}
	cmd := exec.Command("taskkill", "/IM", image, "/F")
	if err := cmd.Run(); err != nil {
		// taskkill returns 128 when the process isn't running.
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 128 {
			return nil
		}
		return fmt.Errorf("taskkill: %w", err)
	}
	time.Sleep(1 * time.Second)
	return nil
}

// LaunchKiroIDE starts the Kiro IDE so it picks up the injected token.
// Only the IDE is auto-launched; the CLI is started by the user on demand.
func LaunchKiroIDE() error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", "-a", "Kiro").Start()
	case "windows":
		return exec.Command("cmd", "/C", "start", "", "Kiro.exe").Start()
	default: // linux
		if path, err := exec.LookPath("kiro"); err == nil {
			return exec.Command(path).Start()
		}
		return fmt.Errorf("kiro binary not found in PATH")
	}
}
