package scout

import (
	"context"
	"strings"
	"testing"

	keelconfig "github.com/nauticana/keel/config"
	keelmodel "github.com/nauticana/keel/model"
	keelport "github.com/nauticana/keel/port"
	keelschema "github.com/nauticana/keel/schema"
)

type scoutConfigQueryFake struct {
	keelport.QueryService
	args []any
	rows [][]any
}

func (f *scoutConfigQueryFake) Query(_ context.Context, _ string, args ...any) (*keelmodel.QueryResult, error) {
	f.args = args
	return &keelmodel.QueryResult{Rows: f.rows}, nil
}

type scoutConfigDatabaseFake struct {
	keelport.DatabaseRepository
	query keelport.QueryService
}

func (f scoutConfigDatabaseFake) GetQueryService(context.Context, map[string]string) keelport.QueryService {
	return f.query
}

func TestScoutConfigApply(t *testing.T) {
	rows := scoutConfigRows()
	rows[agent_max_tokens] = keelconfig.ConfigRow{Value: "16384", Default: "8192"}
	rows[agent_model_capacity_pool] = keelconfig.ConfigRow{Value: " dedicated ", Default: "shared"}

	cfg := &ScoutConfig{}
	if err := cfg.Apply(rows); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if cfg.AgentMaxTokens != 16384 {
		t.Fatalf("AgentMaxTokens = %d, want 16384", cfg.AgentMaxTokens)
	}
	if cfg.AgentTemperature != 0.7 {
		t.Fatalf("AgentTemperature = %v, want 0.7", cfg.AgentTemperature)
	}
	if cfg.AgentModelCapacityPool != "dedicated" {
		t.Fatalf("AgentModelCapacityPool = %q, want dedicated", cfg.AgentModelCapacityPool)
	}
	if cfg.AgentModelMaxWaiters != 4096 {
		t.Fatalf("AgentModelMaxWaiters = %d, want 4096", cfg.AgentModelMaxWaiters)
	}
}

func TestScoutConfigApplyReportsAllInvalidRows(t *testing.T) {
	rows := scoutConfigRows()
	delete(rows, agent_turn_burst)
	rows[agent_model_rate] = keelconfig.ConfigRow{Default: "many"}

	err := (&ScoutConfig{}).Apply(rows)
	if err == nil {
		t.Fatal("Apply returned nil, want catalog errors")
	}
	for _, want := range []string{agent_turn_burst, agent_model_rate} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Apply error %q does not contain %q", err, want)
		}
	}
}

func TestScoutConfigAccessor(t *testing.T) {
	previous := Config()
	t.Cleanup(func() { SetConfig(previous) })

	want := &ScoutConfig{AgentMaxTokens: 42}
	SetConfig(want)
	if got := Config(); got != want {
		t.Fatalf("Config() = %p, want %p", got, want)
	}
}

func TestLoadConfigPublishesKeelAndScoutSections(t *testing.T) {
	previousKeel, previousScout := keelconfig.Config(), Config()
	t.Cleanup(func() {
		keelconfig.SetConfig(previousKeel)
		SetConfig(previousScout)
	})

	oldKeel := &keelconfig.KeelConfig{HttpApiPort: 1}
	oldScout := &ScoutConfig{AgentMaxTokens: 1}
	keelconfig.SetConfig(oldKeel)
	SetConfig(oldScout)
	query := &scoutConfigQueryFake{rows: allConfigQueryRows(t)}

	if err := LoadConfig(context.Background(), scoutConfigDatabaseFake{query: query}, 37); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(query.args) != 1 || query.args[0] != 37 {
		t.Fatalf("query args = %#v, want [37]", query.args)
	}
	if got := keelconfig.Config(); got == oldKeel || got.HttpApiPort != 8080 {
		t.Fatalf("keel Config() = %#v, want newly published defaults", got)
	}
	if got := Config(); got == oldScout || got.AgentMaxTokens != 8192 {
		t.Fatalf("scout Config() = %#v, want newly published defaults", got)
	}
}

func TestLoadConfigFailurePublishesNeitherSection(t *testing.T) {
	previousKeel, previousScout := keelconfig.Config(), Config()
	t.Cleanup(func() {
		keelconfig.SetConfig(previousKeel)
		SetConfig(previousScout)
	})

	oldKeel := &keelconfig.KeelConfig{HttpApiPort: 1}
	oldScout := &ScoutConfig{AgentMaxTokens: 1}
	keelconfig.SetConfig(oldKeel)
	SetConfig(oldScout)
	rows := allConfigQueryRows(t)
	for i, row := range rows {
		if row[0] == agent_turn_burst {
			rows = append(rows[:i], rows[i+1:]...)
			break
		}
	}

	err := LoadConfig(context.Background(), scoutConfigDatabaseFake{query: &scoutConfigQueryFake{rows: rows}}, 0)
	if err == nil || !strings.Contains(err.Error(), agent_turn_burst) {
		t.Fatalf("LoadConfig error = %v, want missing %s", err, agent_turn_burst)
	}
	if got := keelconfig.Config(); got != oldKeel {
		t.Fatalf("keel Config() changed after failed load: got %p, want %p", got, oldKeel)
	}
	if got := Config(); got != oldScout {
		t.Fatalf("scout Config() changed after failed load: got %p, want %p", got, oldScout)
	}
}

func scoutConfigRows() keelconfig.ConfigRows {
	defaults := map[string]string{
		agent_max_tokens:          "8192",
		agent_temperature:         "0.7",
		agent_run_retention_days:  "0",
		agent_turn_rate:           "2",
		agent_turn_burst:          "10",
		agent_tool_rate:           "10",
		agent_tool_burst:          "20",
		agent_model_rate:          "2",
		agent_model_burst:         "5",
		agent_fleet_turn_rate:     "100",
		agent_fleet_turn_burst:    "200",
		agent_fleet_tool_rate:     "500",
		agent_fleet_tool_burst:    "1000",
		agent_fleet_model_rate:    "100",
		agent_fleet_model_burst:   "200",
		agent_max_tenants:         "4096",
		agent_model_capacity_pool: "shared",
		agent_model_capacity:      "32",
		agent_model_max_waiters:   "4096",
	}
	rows := make(keelconfig.ConfigRows, len(defaults))
	for id, value := range defaults {
		rows[id] = keelconfig.ConfigRow{Default: value}
	}
	return rows
}

func allConfigQueryRows(t *testing.T) [][]any {
	t.Helper()
	rows, err := keelschema.ConfigDefaults()
	if err != nil {
		t.Fatalf("keel config defaults: %v", err)
	}
	for id, row := range scoutConfigRows() {
		rows[id] = row
	}
	queryRows := make([][]any, 0, len(rows))
	for id, row := range rows {
		queryRows = append(queryRows, []any{id, row.Value, row.Default})
	}
	return queryRows
}
