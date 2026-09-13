package agentcore

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
)

// scenarios collects every named [agenttest.Scenario] this package's tests
// launch through [agenttest.FakeRuntime]. Files that need a scenario beyond
// the built-in [agenttest.OutputScenario] register it here via their own
// init function, including the unix-only scenarios forkperturn_unix_test.go
// adds when that file's build tag admits it.
var scenarios = map[string]agenttest.Scenario{}

func TestMain(m *testing.M) {
	agenttest.Main(m, scenarios)
}
