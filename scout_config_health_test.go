package scout

import (
	"strings"
	"testing"
	"time"

	keelconfig "github.com/nauticana/keel/config"

	"github.com/nauticana/scout/domain"
)

func TestHealthFlagsTakeTheirDefaultsAndRefuseOutOfRange(t *testing.T) {
	cfg := &ScoutConfig{}
	if err := cfg.Apply(scoutConfigRows()); err != nil {
		t.Fatal(err)
	}
	want := domain.AgentHealthSettings{Period: time.Minute, Batch: 100, WindowMinutes: 60, StalePeriods: 3, AlertPageSize: 50, AlertMaxPageSize: 200}
	if got := cfg.AgentHealthSettings(); got != want {
		t.Fatalf("settings = %+v, want %+v", got, want)
	}
	for flag, value := range map[string]string{
		agent_health_period: "-1", agent_health_batch: "10001", agent_health_window: "10081",
		agent_health_stale_periods: "1001", agent_alert_page_size: "-2", agent_alert_max_page_size: "1001",
	} {
		rows := scoutConfigRows()
		rows[flag] = keelconfig.ConfigRow{Value: value}
		if err := (&ScoutConfig{}).Apply(rows); err == nil || !strings.Contains(err.Error(), flag) {
			t.Errorf("%s=%s: Apply error = %v, want a refusal naming the flag", flag, value, err)
		}
	}
	rows := scoutConfigRows()
	rows[agent_alert_page_size] = keelconfig.ConfigRow{Value: "300"}
	if err := (&ScoutConfig{}).Apply(rows); err == nil || !strings.Contains(err.Error(), agent_alert_page_size) {
		t.Fatalf("a default page above the largest page must be refused: %v", err)
	}
	rows = scoutConfigRows()
	for _, flag := range []string{agent_health_period, agent_health_batch, agent_health_window, agent_health_stale_periods, agent_alert_page_size, agent_alert_max_page_size} {
		rows[flag] = keelconfig.ConfigRow{Value: "0"}
	}
	zero := &ScoutConfig{}
	if err := zero.Apply(rows); err != nil || zero.AgentHealthSettings() != (domain.AgentHealthSettings{}) {
		t.Fatalf("zero flags pass through for the service to default: %+v, %v", zero.AgentHealthSettings(), err)
	}
}
