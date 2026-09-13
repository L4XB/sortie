package clientprotocol

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
)

// fakeRuntimeScenarios collects every [agenttest.Scenario] this
// package's tests launch through [agenttest.FakeRuntime]. Platform-
// specific test files register their own entries from an init
// function; TestMain hands the assembled map to [agenttest.Main]
// unconditionally, so a scenario absent from a given build is simply
// never dispatched.
var fakeRuntimeScenarios = map[string]agenttest.Scenario{}

func TestMain(m *testing.M) {
	agenttest.Main(m, fakeRuntimeScenarios)
}
