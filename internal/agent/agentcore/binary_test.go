package agentcore

import (
	"os"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

func TestResolveBinary(t *testing.T) {
	t.Parallel()

	t.Run("binary on PATH", func(t *testing.T) {
		t.Parallel()
		bin, err := os.Executable()
		if err != nil {
			t.Fatalf("os.Executable() unexpected error: %v", err)
		}

		got, agentErr := ResolveBinary(bin)

		if agentErr != nil {
			t.Fatalf("ResolveBinary(%q) unexpected error: %v", bin, agentErr)
		}
		if got == "" {
			t.Errorf("ResolveBinary(%q) = %q, want non-empty path", bin, got)
		}
	})

	tests := []struct {
		name     string
		command  string
		wantKind domain.AgentErrorKind
		wantMsg  string
	}{
		{
			name:     "binary not found",
			command:  "sortie-no-such-binary-xyzzy",
			wantKind: domain.ErrAgentNotFound,
			wantMsg:  `agent command "sortie-no-such-binary-xyzzy" not found`,
		},
		{
			name:     "multi-token command with space",
			command:  "codex app-server",
			wantKind: domain.ErrAgentNotFound,
			wantMsg:  `agent command must be a single token, got "codex app-server"`,
		},
		{
			name:     "multi-token command with tab",
			command:  "codex\tapp-server",
			wantKind: domain.ErrAgentNotFound,
			wantMsg:  "agent command must be a single token, got \"codex\\tapp-server\"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, agentErr := ResolveBinary(tt.command)

			if agentErr == nil {
				t.Fatalf("ResolveBinary(%q) = %q, want error with kind %q", tt.command, got, tt.wantKind)
			}
			if agentErr.Kind != tt.wantKind {
				t.Errorf("AgentError.Kind = %q, want %q", agentErr.Kind, tt.wantKind)
			}
			if tt.wantMsg != "" && agentErr.Message != tt.wantMsg {
				t.Errorf("AgentError.Message = %q, want %q", agentErr.Message, tt.wantMsg)
			}
		})
	}
}
