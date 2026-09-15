package server

import (
	"bytes"
	"encoding/json"
	"html/template"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/registry"
)

// renderDashboard renders data through the template [New] serves.
func renderDashboard(t *testing.T, data dashboardData) string {
	t.Helper()

	tmpl := template.Must(
		template.New("dashboard").Funcs(template.FuncMap{
			"fmtInt":  FormatInt,
			"fmtCost": FormatCost,
			"even":    func(i int) bool { return i%2 == 0 },
		}).Parse(dashboardHTML),
	)
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		t.Fatalf("dashboard template Execute: %v", err)
	}
	return buf.String()
}

// TestUsageDeclaration_NoneArrivalDiscardedEndToEnd drives a none and an
// incremental session through HandleAgentEvent and RuntimeSnapshot into both
// presenters, since a hand-built SnapshotRunningEntry would bypass the
// accounting under test.
func TestUsageDeclaration_NoneArrivalDiscardedEndToEnd(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 24, 12, 0, 0, 0, time.UTC)
	state := orchestrator.NewState(60000, 4, 0, nil, orchestrator.AgentTotals{})

	state.Running["id-none"] = &orchestrator.RunningEntry{
		Identifier:       "USG-NONE",
		Issue:            domain.Issue{ID: "id-none", Identifier: "USG-NONE", State: "In Progress"},
		StartedAt:        now.Add(-5 * time.Minute),
		AgentKind:        "kind-none",
		UsageArrival:     registry.UsageArrivalNone,
		UsageAttribution: registry.UsageAttributionNone,
	}
	state.Running["id-inc"] = &orchestrator.RunningEntry{
		Identifier:       "USG-INC",
		Issue:            domain.Issue{ID: "id-inc", Identifier: "USG-INC", State: "In Progress"},
		StartedAt:        now.Add(-5 * time.Minute),
		AgentKind:        "kind-inc",
		UsageArrival:     registry.UsageArrivalIncremental,
		UsageAttribution: registry.UsageAttributionPerModel,
	}

	usage := domain.TokenUsage{InputTokens: 2_000_000, OutputTokens: 1_000_000, TotalTokens: 3_000_000, CacheReadTokens: 500_000}
	orchestrator.HandleAgentEvent(state, "id-none", domain.AgentEvent{
		Type: domain.EventTokenUsage, Timestamp: now, Model: "none-model", Usage: usage,
	}, slog.Default(), nil)
	orchestrator.HandleAgentEvent(state, "id-inc", domain.AgentEvent{
		Type: domain.EventTokenUsage, Timestamp: now, Model: "inc-model", Usage: usage,
	}, slog.Default(), nil)

	snap := orchestrator.RuntimeSnapshot(state, now)

	fptr := func(v float64) *float64 { return &v }
	rates := TokenRates{
		"kind-none": {InputPerMtok: fptr(3.0), OutputPerMtok: fptr(15.0)},
		"kind-inc":  {InputPerMtok: fptr(3.0), OutputPerMtok: fptr(15.0)},
	}
	const wantCost = "$21.00" // 2M input @ $3/Mtok + 1M output @ $15/Mtok

	data := buildDashboardData(snap, "test", now.Add(-time.Hour), nil, now, rates)

	var noneRow, incRow dashboardRunningEntry
	var foundNone, foundInc bool
	for _, r := range data.Running {
		switch r.Identifier {
		case "USG-NONE":
			noneRow, foundNone = r, true
		case "USG-INC":
			incRow, foundInc = r, true
		}
	}
	if !foundNone || !foundInc {
		t.Fatalf("dashboard rows = %+v, want rows for both USG-NONE and USG-INC", data.Running)
	}

	if noneRow.ModelRow != dashPlaceholder {
		t.Errorf("none session ModelRow = %q, want %q", noneRow.ModelRow, dashPlaceholder)
	}
	if noneRow.APIRequestsRow != dashPlaceholder {
		t.Errorf("none session APIRequestsRow = %q, want %q", noneRow.APIRequestsRow, dashPlaceholder)
	}
	if noneRow.TokensRow != dashPlaceholder {
		t.Errorf("none session TokensRow = %q, want %q", noneRow.TokensRow, dashPlaceholder)
	}
	if noneRow.EstCostRow != dashPlaceholder {
		t.Errorf("none session EstCostRow = %q, want %q", noneRow.EstCostRow, dashPlaceholder)
	}
	if incRow.EstimatedCostUSD != wantCost {
		t.Fatalf("incremental session EstimatedCostUSD = %q, want %q (setup invariant)", incRow.EstimatedCostUSD, wantCost)
	}

	if data.TotalTokens != usage.TotalTokens {
		t.Errorf("Total Tokens card = %d, want %d (the incremental session's figure alone)", data.TotalTokens, usage.TotalTokens)
	}
	if data.InputTokens != usage.InputTokens || data.OutputTokens != usage.OutputTokens || data.CacheReadTokens != usage.CacheReadTokens {
		t.Errorf("footer token totals = (%d, %d, %d), want (%d, %d, %d)",
			data.InputTokens, data.OutputTokens, data.CacheReadTokens,
			usage.InputTokens, usage.OutputTokens, usage.CacheReadTokens)
	}
	if data.EstimatedCostUSD == nil || *data.EstimatedCostUSD != wantCost {
		t.Errorf("Active Est. Cost card = %v, want %q", data.EstimatedCostUSD, wantCost)
	}

	html := renderDashboard(t, data)
	if strings.Contains(html, "none-model") {
		t.Error("rendered dashboard contains the none session's discarded model name")
	}
	if !strings.Contains(html, "inc-model") {
		t.Error("rendered dashboard is missing the incremental session's model name")
	}

	resp := toStateResponse(snap, rates)

	var noneResp, incResp runningEntryResponse
	var foundNoneResp, foundIncResp bool
	for _, r := range resp.Running {
		switch r.IssueIdentifier {
		case "USG-NONE":
			noneResp, foundNoneResp = r, true
		case "USG-INC":
			incResp, foundIncResp = r, true
		}
	}
	if !foundNoneResp || !foundIncResp {
		t.Fatalf("state response running rows = %+v, want rows for both USG-NONE and USG-INC", resp.Running)
	}
	if incResp.ModelName != "inc-model" {
		t.Fatalf("incremental session model_name = %q, want %q (setup invariant)", incResp.ModelName, "inc-model")
	}

	if noneResp.UsageArrival != string(registry.UsageArrivalNone) {
		t.Errorf("none session usage_arrival = %q, want %q", noneResp.UsageArrival, registry.UsageArrivalNone)
	}
	if noneResp.TokensMeasured {
		t.Error("none session tokens_measured = true, want false")
	}
	if noneResp.Tokens.InputTokens != nil || noneResp.Tokens.OutputTokens != nil ||
		noneResp.Tokens.TotalTokens != nil || noneResp.Tokens.CacheReadTokens != nil {
		t.Errorf("none session tokens = %+v, want all four nil", noneResp.Tokens)
	}
	if noneResp.APIRequestCount != nil {
		t.Errorf("none session api_request_count = %v, want nil", noneResp.APIRequestCount)
	}
	if noneResp.ModelName != "" {
		t.Errorf("none session model_name = %q, want empty", noneResp.ModelName)
	}

	if resp.AgentTotals.TotalTokens != usage.TotalTokens {
		t.Errorf("agent_totals.total_tokens = %d, want %d", resp.AgentTotals.TotalTokens, usage.TotalTokens)
	}
	if resp.ActiveEstimatedCostUSD == nil || *resp.ActiveEstimatedCostUSD != 21.0 {
		t.Errorf("active_estimated_cost_usd = %v, want 21", resp.ActiveEstimatedCostUSD)
	}

	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("json.Marshal(stateResponse): %v", err)
	}
	var parsed struct {
		Running []map[string]any `json:"running"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	var noneWire map[string]any
	for _, row := range parsed.Running {
		if row["issue_identifier"] == "USG-NONE" {
			noneWire = row
			break
		}
	}
	if noneWire == nil {
		t.Fatal("marshaled response has no running row for USG-NONE")
	}
	if v, present := noneWire["model_name"]; present {
		t.Errorf("marshaled none session carries a model_name member: %v", v)
	}
	tokens, ok := noneWire["tokens"].(map[string]any)
	if !ok {
		t.Fatalf("marshaled none session tokens = %v, want an object", noneWire["tokens"])
	}
	for _, key := range []string{"input_tokens", "output_tokens", "total_tokens", "cache_read_tokens"} {
		if v, present := tokens[key]; !present || v != nil {
			t.Errorf("marshaled none session tokens.%s = %v (present=%v), want null", key, v, present)
		}
	}
	if v, present := noneWire["api_request_count"]; !present || v != nil {
		t.Errorf("marshaled none session api_request_count = %v (present=%v), want null", v, present)
	}
}
