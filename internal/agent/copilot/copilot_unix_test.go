//go:build unix

package copilot

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
)

// versionHeldDescendantScenario names the fake copilot binary L2's
// fixture uses: on a "--version" invocation it starts an already-built
// descendant inheriting its own standard output and standard error,
// records the descendant's pid, then prints a version line and exits
// 0.
const versionHeldDescendantScenario = "copilot.version-held-descendant"

type versionHeldDescendantParams struct {
	ChildPath    string
	ChildPIDPath string
}

func runVersionHeldDescendant(args []string, p versionHeldDescendantParams) int {
	if len(args) == 0 || args[0] != "--version" {
		return 0
	}
	if err := startHeldDescendant(p.ChildPath, p.ChildPIDPath); err != nil {
		fmt.Fprintf(os.Stderr, "version held descendant: %v\n", err)
		return 2
	}
	fmt.Println("copilot version 1.2.3")
	return 0
}

// ghAuthHeldDescendantScenario names the fake gh binary L3's fixture
// uses: on an "auth status" invocation it starts an already-built
// descendant the same way, then exits 0.
const ghAuthHeldDescendantScenario = "copilot.gh-auth-held-descendant"

type ghAuthHeldDescendantParams struct {
	ChildPath    string
	ChildPIDPath string
}

func runGhAuthHeldDescendant(args []string, p ghAuthHeldDescendantParams) int {
	if len(args) < 2 || args[0] != "auth" || args[1] != "status" {
		return 0
	}
	if err := startHeldDescendant(p.ChildPath, p.ChildPIDPath); err != nil {
		fmt.Fprintf(os.Stderr, "gh auth held descendant: %v\n", err)
		return 2
	}
	return 0
}

func startHeldDescendant(childPath, childPIDPath string) error {
	cmd := exec.Command(childPath) //nolint:gosec // fake runtime path this scenario was handed
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	return os.WriteFile(childPIDPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)
}

func init() {
	fakeScenarios[versionHeldDescendantScenario] = agenttest.Typed(runVersionHeldDescendant)
	fakeScenarios[ghAuthHeldDescendantScenario] = agenttest.Typed(runGhAuthHeldDescendant)
}

// pollCopilotPIDAndAssertGone polls path for a positive PID, then
// polls until kill(pid, 0) reports an error (process gone), failing t
// if either bound is exceeded.
func pollCopilotPIDAndAssertGone(t *testing.T, path string) {
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

// TestStartSession_VersionCanaryHeldDescendantHoldingOutput pins P9
// for L2: with a held descendant holding the version canary's output,
// StartSession returns within its timer with the canary passing, and
// the descendant is gone.
func TestStartSession_VersionCanaryHeldDescendantHoldingOutput(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	dir := t.TempDir()
	descendantPath := agenttest.FakeRuntime(t, dir, "descendant", agenttest.OutputScenario, agenttest.Output{Hang: true})
	pidPath := filepath.Join(dir, "child.pid")
	binPath := agenttest.FakeRuntime(t, dir, "copilot", versionHeldDescendantScenario, versionHeldDescendantParams{
		ChildPath:    descendantPath,
		ChildPIDPath: pidPath,
	})

	adapter, err := NewCopilotAdapter(map[string]any{})
	if err != nil {
		t.Fatalf("NewCopilotAdapter() error = %v", err)
	}

	start := time.Now()
	_, err = adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: binPath},
	})
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("StartSession() took %v, want within 3s", elapsed)
	}
	if err != nil {
		t.Fatalf("StartSession() error = %v, want nil", err)
	}

	pollCopilotPIDAndAssertGone(t, pidPath)
}

// TestCheckAuth_HeldDescendantHoldingOutput pins P9 for L3: with a
// held descendant holding the "gh auth status" output, checkAuth
// returns within its timer reporting authentication present, and the
// descendant is gone.
func TestCheckAuth_HeldDescendantHoldingOutput(t *testing.T) {
	for _, env := range []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		t.Setenv(env, "")
	}

	dir := t.TempDir()
	descendantPath := agenttest.FakeRuntime(t, dir, "descendant", agenttest.OutputScenario, agenttest.Output{Hang: true})
	pidPath := filepath.Join(dir, "child.pid")
	agenttest.FakeRuntime(t, dir, "gh", ghAuthHeldDescendantScenario, ghAuthHeldDescendantParams{
		ChildPath:    descendantPath,
		ChildPIDPath: pidPath,
	})
	t.Setenv("PATH", dir)

	start := time.Now()
	err := checkAuth(context.Background(), 5*time.Second)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("checkAuth() took %v, want within 3s", elapsed)
	}
	if err != nil {
		t.Fatalf("checkAuth() = %v, want nil", err)
	}

	pollCopilotPIDAndAssertGone(t, pidPath)
}
