//go:build unix

package procutil

import (
	"errors"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
)

// signalledErr starts a hanging fake-runtime subprocess, kills it with
// SIGKILL, and returns the resulting *exec.ExitError.
func signalledErr(t *testing.T) error {
	t.Helper()
	cmd := fakeRuntimeCmd(t, agenttest.Output{Hang: true})
	if err := cmd.Start(); err != nil {
		t.Fatalf("signalledErr: Start: %v", err)
	}
	_ = cmd.Process.Kill()
	err := cmd.Wait()
	if err == nil {
		t.Fatal("signalledErr: expected error from killed process, got nil")
	}
	return err
}

func TestWasSignaled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		makeErr func(t *testing.T) error
		want    bool
	}{
		{
			name:    "nil error returns false",
			makeErr: func(_ *testing.T) error { return nil },
			want:    false,
		},
		{
			name:    "process killed by signal returns true",
			makeErr: signalledErr,
			want:    true,
		},
		{
			name: "normal exit returns false",
			makeErr: func(t *testing.T) error {
				t.Helper()
				return fakeRuntimeCmd(t, agenttest.Output{ExitCode: 1}).Run()
			},
			want: false,
		},
		{
			name:    "non-ExitError returns false",
			makeErr: func(_ *testing.T) error { return errors.New("not an exit error") },
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := WasSignaled(tt.makeErr(t))
			if got != tt.want {
				t.Errorf("WasSignaled() = %v, want %v", got, tt.want)
			}
		})
	}
}
