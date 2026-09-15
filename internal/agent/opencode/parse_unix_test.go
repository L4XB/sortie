//go:build unix

package opencode

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
)

// pollOpencodePIDAndAssertGone polls path for a positive PID, then
// polls until kill(pid, 0) reports an error (process gone), failing t
// if either bound is exceeded.
func pollOpencodePIDAndAssertGone(t *testing.T, path string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	var pid int
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			if v, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil && v > 0 {
				pid = v
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatalf("pid file %q never populated", path)
	}

	goneDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(goneDeadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("descendant %d still answers signal 0, want it gone", pid)
}

// TestQueryExportUsage_HeldDescendantHoldingOutput pins P9 for L4: with
// a held descendant holding the export query's output, queryExportUsage
// returns within its timer with the export's usage, and the descendant
// is gone.
func TestQueryExportUsage_HeldDescendantHoldingOutput(t *testing.T) {
	tmpDir := t.TempDir()
	fixturePath := filepath.Join(tmpDir, "export_usage.json")
	if err := os.WriteFile(fixturePath, loadFixture(t, "export_usage.json"), 0o644); err != nil { //nolint:gosec // fixture file under t.TempDir()
		t.Fatalf("WriteFile(export_usage.json): %v", err)
	}
	pidPath := filepath.Join(tmpDir, "descendant.pid")

	body := "cat '" + fixturePath + "'\n" +
		"sleep 30 & echo $! > '" + pidPath + "'\n"
	script := agenttest.WriteScript(t, tmpDir, "fake-export", body)
	state := testExportState(script, tmpDir)

	start := time.Now()
	usage := queryExportUsage(context.Background(), state, 0)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("queryExportUsage() took %v, want within 3s", elapsed)
	}
	if usage.InputTokens != 1750 {
		t.Errorf("InputTokens = %d, want 1750", usage.InputTokens)
	}
	if usage.OutputTokens != 300 {
		t.Errorf("OutputTokens = %d, want 300", usage.OutputTokens)
	}

	pollOpencodePIDAndAssertGone(t, pidPath)
}

// TestQueryModelNotFound_HeldDescendantHoldingOutput pins P9 for L5:
// with a held descendant holding the models query's output,
// queryModelNotFound returns within its timer reporting the configured
// model present in the catalog (ok false), and the descendant is gone.
func TestQueryModelNotFound_HeldDescendantHoldingOutput(t *testing.T) {
	tmpDir := t.TempDir()
	pidPath := filepath.Join(tmpDir, "descendant.pid")

	body := "printf 'anthropic/claude-sonnet-4-5\\nopenai/gpt-5\\n'\n" +
		"sleep 30 & echo $! > '" + pidPath + "'\n"
	script := agenttest.WriteScript(t, tmpDir, "fake-models", body)

	state := &sessionState{
		target: agentcore.LaunchTarget{
			Command:       script,
			WorkspacePath: tmpDir,
		},
		baseLogger: slog.Default(),
	}
	state.passthrough.Model = "anthropic/claude-sonnet-4-5"

	start := time.Now()
	message, ok := queryModelNotFound(context.Background(), state)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("queryModelNotFound() took %v, want within 3s", elapsed)
	}
	if ok {
		t.Errorf("ok = true (message %q), want false: the configured model is present in the catalog", message)
	}

	pollOpencodePIDAndAssertGone(t, pidPath)
}

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

	t.Run("the_warning_reports_an_export_that_recovered_nothing", func(t *testing.T) {
		t.Parallel()

		// A finished step whose provider reported zero is a measurement, so
		// saying no usage was found contradicts the verdict it produces. The
		// unfinished placeholder recovers nothing and must still say so.
		for name, tc := range map[string]struct {
			finish   string
			wantWarn bool
		}{
			"finished_zero_step": {finish: `"finish":"stop",`, wantWarn: false},
			"unfinished_step":    {finish: ``, wantWarn: true},
		} {
			tmpDir := t.TempDir()
			exportPath := filepath.Join(tmpDir, "export.json")
			export := `{"messages":[{"info":{"role":"assistant","sessionID":"ses_abc123",` + tc.finish +
				`"tokens":{"input":0,"output":0,"reasoning":0,"cache":{"read":0,"write":0}}}}]}`
			if err := os.WriteFile(exportPath, []byte(export), 0o644); err != nil { //nolint:gosec // fixture file under t.TempDir()
				t.Fatal(err)
			}
			script := agenttest.WriteScript(t, tmpDir, "fake-export", "cat '"+exportPath+"'")

			var logs bytes.Buffer
			state := testExportState(script, tmpDir)
			state.baseLogger = slog.New(slog.NewTextHandler(&logs, nil))

			queryExportUsage(context.Background(), state, 0)

			if got := strings.Contains(logs.String(), "no assistant token usage found"); got != tc.wantWarn {
				t.Errorf("%s: warned = %v, want %v", name, got, tc.wantWarn)
			}
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
