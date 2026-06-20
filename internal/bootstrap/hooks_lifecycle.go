package bootstrap

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/szymonrychu/tatara-claude-code-wrapper/internal/metrics"
)

// HookRunner executes a lifecycle-hook command via `sh -c` in dir, with posArgs
// passed as shell positional parameters ($1, $2, ...) and extraEnv appended to
// the process environment. Injected for testability.
type HookRunner func(dir, command string, posArgs, extraEnv []string) error

// DefaultHookRunner is the production HookRunner: it runs `sh -c <command>` with
// the inherited environment plus extraEnv. A fixed $0 label ("tatara-hook") is
// inserted so the first real argument lands in $1, matching shell convention.
func DefaultHookRunner(dir, command string, posArgs, extraEnv []string) error {
	args := append([]string{"-c", command, "tatara-hook"}, posArgs...)
	cmd := exec.Command("sh", args...) //nolint:gosec // command is operator-supplied Project config (CRD), not end-user input
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), extraEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// RunHook runs a single lifecycle hook best-effort. It is a no-op when the
// command is empty or no runner is wired. Otherwise it executes the command,
// counts the outcome (LifecycleHookTotal{result,hook}) and logs it; a failure is
// logged at WARN and never returned, so a broken hook can never abort the agent
// run (matching the best-effort InstallHooks contract).
func RunHook(name, command, dir string, posArgs, extraEnv []string, run HookRunner, log *slog.Logger, m *metrics.Metrics) {
	if command == "" || run == nil {
		return
	}
	start := time.Now()
	err := run(dir, command, posArgs, extraEnv)
	result := "ok"
	if err != nil {
		result = "fail"
	}
	if m != nil {
		m.LifecycleHookTotal.WithLabelValues(result, name).Inc()
	}
	if log != nil {
		if err != nil {
			log.Warn("lifecycle hook failed (best-effort)", "action", "lifecycle_hook", "hook", name, "dir", dir, "error", err, "duration_ms", time.Since(start).Milliseconds())
		} else {
			log.Info("lifecycle hook ok", "action", "lifecycle_hook", "hook", name, "dir", dir, "duration_ms", time.Since(start).Milliseconds())
		}
	}
}
