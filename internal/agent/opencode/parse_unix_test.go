//go:build unix

package opencode

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
)

func writeExportScript(t *testing.T, dir, fixtureName string, exitCode int) (string, string) {
	t.Helper()

	argsPath := filepath.Join(dir, "args.log")
	body := `printf '%s\n' "$@" > '` + argsPath + `'
exit ` + strconv.Itoa(exitCode)
	if fixtureName != "" && exitCode == 0 {
		fixturePath := filepath.Join(dir, fixtureName)
		if err := os.WriteFile(fixturePath, loadFixture(t, fixtureName), 0o644); err != nil {
			t.Fatalf("WriteFile(%q): %v", fixtureName, err)
		}
		body = `printf '%s\n' "$@" > '` + argsPath + `'
cat '` + fixturePath + `'`
	}

	return agenttest.WriteScript(t, dir, "fake-export", body), argsPath
}

func testExportState(command, workspace string) *sessionState {
	return &sessionState{
		target: agentcore.LaunchTarget{
			Command:       command,
			WorkspacePath: workspace,
		},
		sessionID:  "ses_abc123",
		baseLogger: slog.Default(),
	}
}

func TestQueryExportSubprocess(t *testing.T) {
	t.Parallel()

	t.Run("a_cancelled_turn_context_does_not_cost_the_recovery", func(t *testing.T) {
		t.Parallel()

		// Every terminal path hands `recoverUsage` the TURN's context, and on
		// the cancel path that context is the thing that just fired. The read
		// timeout and the process-exit path can reach it after a cancellation
		// too. The export is terminal work about a turn that already ran, so
		// it must not inherit the caller's cancellation.
		tmpDir := t.TempDir()
		script, _ := writeExportScript(t, tmpDir, "export_usage.json", 0)
		state := testExportState(script, tmpDir)

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		// The hazard, measured rather than assumed: an attached query on this
		// context recovers nothing at all.
		if usage := queryExportUsage(ctx, state, 0); hasUsage(usage) {
			t.Fatal("queryExportUsage recovered on a cancelled context; this test's premise is gone")
		}

		recovered := recoverUsage(ctx, state, 0)
		if recovered == nil {
			t.Fatal("recoverUsage = nil on a cancelled turn context, want the export's figures")
		}
		if recovered.Run.InputTokens != 1750 || recovered.Run.OutputTokens != 300 {
			t.Errorf("Run = in:%d out:%d, want in:1750 out:300",
				recovered.Run.InputTokens, recovered.Run.OutputTokens)
		}
	})

	t.Run("local_subprocess_usage_extracted", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		script, argsPath := writeExportScript(t, tmpDir, "export_usage.json", 0)
		state := testExportState(script, tmpDir)

		usage := queryExportUsage(context.Background(), state, 0)
		if usage.InputTokens != 1750 {
			t.Errorf("InputTokens = %d, want 1750", usage.InputTokens)
		}
		if usage.OutputTokens != 300 {
			t.Errorf("OutputTokens = %d, want 300", usage.OutputTokens)
		}
		if usage.CacheReadTokens != 200 {
			t.Errorf("CacheReadTokens = %d, want 200", usage.CacheReadTokens)
		}

		args, err := os.ReadFile(argsPath)
		if err != nil {
			t.Fatalf("ReadFile(args.log): %v", err)
		}
		if string(args) != "export\n--sanitize\nses_abc123\n" {
			t.Errorf("export args = %q, want %q", string(args), "export\n--sanitize\nses_abc123\n")
		}
	})

	t.Run("local_subprocess_missing_tokens_returns_zero", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		script, _ := writeExportScript(t, tmpDir, "export_usage_missing_tokens.json", 0)
		state := testExportState(script, tmpDir)

		usage := queryExportUsage(context.Background(), state, 0)
		if usage != (exportUsage{}) {
			t.Errorf("usage = %+v, want zero value", usage)
		}
	})

	t.Run("local_subprocess_nonzero_exit_returns_zero", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		script, _ := writeExportScript(t, tmpDir, "", 1)
		state := testExportState(script, tmpDir)

		usage := queryExportUsage(context.Background(), state, 0)
		if usage != (exportUsage{}) {
			t.Errorf("usage = %+v, want zero value", usage)
		}
	})

	t.Run("ssh_subprocess_usage_extracted", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		script, argsPath := writeExportScript(t, tmpDir, "export_usage.json", 0)
		state := testExportState(script, tmpDir)
		state.target.RemoteCommand = "opencode"
		state.target.SSHHost = "example.test"

		usage := queryExportUsage(context.Background(), state, 0)
		if usage.InputTokens != 1750 {
			t.Errorf("InputTokens = %d, want 1750", usage.InputTokens)
		}
		if usage.OutputTokens != 300 {
			t.Errorf("OutputTokens = %d, want 300", usage.OutputTokens)
		}
		if usage.CacheReadTokens != 200 {
			t.Errorf("CacheReadTokens = %d, want 200", usage.CacheReadTokens)
		}

		args, err := os.ReadFile(argsPath)
		if err != nil {
			t.Fatalf("ReadFile(args.log): %v", err)
		}
		logged := string(args)
		if !strings.Contains(logged, "example.test") {
			t.Errorf("ssh args = %q, want host %q", logged, "example.test")
		}
		if !strings.Contains(logged, "export") || !strings.Contains(logged, "--sanitize") || !strings.Contains(logged, "ses_abc123") {
			t.Errorf("ssh args = %q, want export invocation details", logged)
		}
	})
}
