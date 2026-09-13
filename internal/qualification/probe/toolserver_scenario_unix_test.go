//go:build unix

package probe

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

// jsonRPCEnvelope reads just enough of a JSON-RPC request line to route
// it: the method name and the id to echo back unmodified.
type jsonRPCEnvelope struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
}

// runMCPToolServer implements mcpToolServerScenario: a fake MCP stdio
// server that records every tools/call it receives to
// params.RecordPath, one line per call, and otherwise answers only
// what a session/new declaring it needs: initialize, tools/list, and
// tools/call. It follows internal/agent/clientprotocol's own
// mcpHandshakeScript shape, adapted from the Agent Client Protocol's
// own wire to the Model Context Protocol's.
func runMCPToolServer(_ []string, params mcpToolServerParams) int {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Bytes()
		var req jsonRPCEnvelope
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		var err error
		switch req.Method {
		case "initialize":
			err = respond(`{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":%q,"version":"0.0.0"}}}`, req.ID, toolServerName)
		case "tools/list":
			err = respond(`{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":%q,"description":"records that the qualification probe induced a call","inputSchema":{"type":"object","properties":{}}}]}}`, req.ID, probeToolName)
		case "tools/call":
			if recordErr := recordToolCall(params.RecordPath, line); recordErr != nil {
				fmt.Fprintf(os.Stderr, "record tool call: %v\n", recordErr)
				return 1
			}
			err = respond(`{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"ok"}]}}`, req.ID)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "respond to %s: %v\n", req.Method, err)
			return 1
		}
	}
	return 0
}

// respond writes one JSON-RPC response line to standard output.
func respond(format string, args ...any) error {
	_, err := fmt.Fprintf(os.Stdout, format+"\n", args...)
	return err
}

// recordToolCall appends line, followed by a newline, to path.
func recordToolCall(path string, line []byte) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // path is under the caller's own t.TempDir()
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // best-effort close after a completed write

	if _, err := f.Write(line); err != nil {
		return err
	}
	_, err = f.WriteString("\n")
	return err
}
