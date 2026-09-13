package agenttest

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Scenario is the body of a fake runtime. It runs inside a re-executed
// copy of the test binary, receives the arguments the runtime was
// launched with and the parameters passed to [FakeRuntime], and returns
// the process exit code.
type Scenario func(args []string, params json.RawMessage) int

// OutputScenario names the built-in [Scenario] that every package can
// launch without registering it. Its parameters are an [Output].
const OutputScenario = "agenttest.output"

// Output parameterizes [OutputScenario]: the runtime writes Stdout, then
// Stderr, and exits with ExitCode, or stays alive until killed when Hang
// is set.
type Output struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Hang     bool
}

type fakeConfig struct {
	Scenario string
	Params   json.RawMessage
}

// Main is the whole TestMain body of a package whose tests call
// [FakeRuntime]. A process started from a fake runtime executable runs
// its scenario from scenarios and exits; any other process runs the
// package's tests.
func Main(m *testing.M, scenarios map[string]Scenario) {
	if exe, err := os.Executable(); err == nil {
		if config, err := os.ReadFile(configPath(exe)); err == nil {
			os.Exit(runScenario(config, scenarios))
		}
	}
	os.Exit(m.Run())
}

// Typed adapts run into a [Scenario] whose parameters decode into P.
func Typed[P any](run func(args []string, params P) int) Scenario {
	return func(args []string, raw json.RawMessage) int {
		var params P
		if err := json.Unmarshal(raw, &params); err != nil {
			fmt.Fprintf(os.Stderr, "fake runtime: decode params: %v\n", err)
			return 2
		}
		return run(args, params)
	}
}

// Hang blocks until the process is killed. It sleeps rather than
// blocking on a channel because a pending timer keeps the Go runtime's
// deadlock detector from ending the process on its own.
func Hang() {
	for {
		time.Sleep(time.Hour)
	}
}

// FakeRuntime creates an executable named name in dir that runs scenario
// with params, and returns its path. On Windows the name gains the .exe
// suffix. The executable is the test binary itself under a new name, so
// the calling package's TestMain must call [Main].
func FakeRuntime(t testing.TB, dir, name, scenario string, params any) string {
	t.Helper()

	encoded, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("FakeRuntime: encode params: %v", err)
	}
	config, err := json.Marshal(fakeConfig{Scenario: scenario, Params: encoded})
	if err != nil {
		t.Fatalf("FakeRuntime: encode config: %v", err)
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("FakeRuntime: %v", err)
	}
	path := filepath.Join(dir, name)
	if runtime.GOOS == "windows" {
		path += ".exe"
	}
	if err := os.WriteFile(configPath(path), config, 0o600); err != nil {
		t.Fatalf("FakeRuntime: %v", err)
	}

	// A hard link never opens the executable for writing, so it cannot
	// hit the ETXTBSY race a freshly written executable meets when another
	// goroutine forks (golang/go#22315). The copy covers a test binary
	// that sits on another file system.
	if err := os.Link(self, path); err != nil {
		if err := copyExecutable(self, path); err != nil {
			t.Fatalf("FakeRuntime: %v", err)
		}
	}
	return path
}

func runScenario(config []byte, scenarios map[string]Scenario) int {
	var cfg fakeConfig
	if err := json.Unmarshal(config, &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "fake runtime: decode config: %v\n", err)
		return 2
	}
	run := scenarios[cfg.Scenario]
	if cfg.Scenario == OutputScenario {
		run = Typed(writeOutput)
	}
	if run == nil {
		fmt.Fprintf(os.Stderr, "fake runtime: unknown scenario %q\n", cfg.Scenario)
		return 2
	}
	return run(os.Args[1:], cfg.Params)
}

func writeOutput(_ []string, out Output) int {
	_, _ = io.WriteString(os.Stdout, out.Stdout)
	_, _ = io.WriteString(os.Stderr, out.Stderr)
	if out.Hang {
		Hang()
	}
	return out.ExitCode
}

func configPath(exe string) string {
	return strings.TrimSuffix(exe, ".exe") + ".fake.json"
}

func copyExecutable(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // src is the running test binary
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck // read-only

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700) //nolint:gosec // dst is under the caller's test directory
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
