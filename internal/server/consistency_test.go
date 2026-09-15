package server

import (
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/registry"
)

// leadingCount extracts the leading integer from note, or -1 when note is empty.
func leadingCount(t *testing.T, note string) int {
	t.Helper()
	if note == "" {
		return -1
	}
	m := regexp.MustCompile(`^(\d+) `).FindStringSubmatch(note)
	if m == nil {
		t.Fatalf("note %q does not start with a count", note)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("parse leading count from %q: %v", note, err)
	}
	return n
}

// TestDashboardAndStateAgreeOnCountsAndCost verifies [buildDashboardData]
// and [toStateResponse] report the same counts and cost for the same
// [orchestrator.RuntimeSnapshotResult].
func TestDashboardAndStateAgreeOnCountsAndCost(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)
	fptr := func(v float64) *float64 { return &v }
	rates := TokenRates{"claude": TokenRateConfig{InputPerMtok: fptr(3.0)}}

	snap := orchestrator.RuntimeSnapshotResult{
		GeneratedAt: now,
		Running: []orchestrator.SnapshotRunningEntry{
			{
				IssueID: "priced", Identifier: "MT-PRICED", StartedAt: now.Add(-time.Minute),
				AgentKind: "claude", UsageMeasured: true, AgentInputTokens: 1_000_000,
				UsageArrival: registry.UsageArrivalIncremental,
			},
			{
				IssueID: "unpriced", Identifier: "MT-UNPRICED", StartedAt: now.Add(-time.Minute),
				AgentKind: "unpriced-kind", UsageMeasured: true, AgentInputTokens: 1_000_000,
				UsageArrival: registry.UsageArrivalIncremental,
			},
			{
				IssueID: "unreported", Identifier: "MT-UNREPORTED", StartedAt: now.Add(-time.Minute),
				UsageArrival: registry.UsageArrivalIncremental,
			},
			{
				IssueID: "nonreporting", Identifier: "MT-NONREPORTING", StartedAt: now.Add(-time.Minute),
				UsageArrival: registry.UsageArrivalNone,
			},
		},
		AgentTotals: orchestrator.SnapshotAgentTotals{
			UnmeasuredSessions:  4,
			RunningUnreported:   1,
			RunningNonReporting: 1,
		},
	}

	stateResp := toStateResponse(snap, rates)
	dashData := buildDashboardData(snap, "v1", now.Add(-time.Hour), nil, now, rates)

	if stateResp.AgentTotals.UnmeasuredSessions != snap.AgentTotals.UnmeasuredSessions {
		t.Errorf("state AgentTotals.UnmeasuredSessions = %d, want %d",
			stateResp.AgentTotals.UnmeasuredSessions, snap.AgentTotals.UnmeasuredSessions)
	}
	if got := leadingCount(t, dashData.EndedUnmeasuredNote); int64(got) != snap.AgentTotals.UnmeasuredSessions {
		t.Errorf("dashboard EndedUnmeasuredNote count = %d, want %d", got, snap.AgentTotals.UnmeasuredSessions)
	}

	if stateResp.AgentTotals.RunningUnreported != snap.AgentTotals.RunningUnreported {
		t.Errorf("state AgentTotals.RunningUnreported = %d, want %d",
			stateResp.AgentTotals.RunningUnreported, snap.AgentTotals.RunningUnreported)
	}
	if got := leadingCount(t, dashData.RunningUnreportedNote); got != snap.AgentTotals.RunningUnreported {
		t.Errorf("dashboard RunningUnreportedNote count = %d, want %d", got, snap.AgentTotals.RunningUnreported)
	}

	if stateResp.AgentTotals.RunningNonReporting != snap.AgentTotals.RunningNonReporting {
		t.Errorf("state AgentTotals.RunningNonReporting = %d, want %d",
			stateResp.AgentTotals.RunningNonReporting, snap.AgentTotals.RunningNonReporting)
	}
	if got := leadingCount(t, dashData.RunningNonReportingNote); got != snap.AgentTotals.RunningNonReporting {
		t.Errorf("dashboard RunningNonReportingNote count = %d, want %d", got, snap.AgentTotals.RunningNonReporting)
	}

	requireCostUnpricedRunning(t, stateResp.CostUnpricedRunning, 1)
	if got := leadingCount(t, dashData.CostUnpricedNote); got != 1 {
		t.Errorf("dashboard CostUnpricedNote count = %d, want 1", got)
	}

	if stateResp.ActiveEstimatedCostUSD == nil {
		t.Fatal("state ActiveEstimatedCostUSD = nil, want a priced total")
	}
	if *stateResp.ActiveEstimatedCostUSD != 3.0 {
		t.Errorf("state ActiveEstimatedCostUSD = %v, want 3.0", *stateResp.ActiveEstimatedCostUSD)
	}
	if dashData.EstimatedCostUSD == nil {
		t.Fatal("dashboard EstimatedCostUSD = nil, want a priced total")
	}
	if *dashData.EstimatedCostUSD != FormatCost(*stateResp.ActiveEstimatedCostUSD) {
		t.Errorf("dashboard EstimatedCostUSD = %q, want %q (formatted from the same total the state API reports)",
			*dashData.EstimatedCostUSD, FormatCost(*stateResp.ActiveEstimatedCostUSD))
	}
}
