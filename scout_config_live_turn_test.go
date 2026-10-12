package scout

import (
	"strings"
	"testing"

	keelconfig "github.com/nauticana/keel/config"

	"github.com/nauticana/scout/domain"
)

func TestLiveTurnFlagsTakeTheirDefaultsAndRefuseOutOfRange(t *testing.T) {
	cfg := &ScoutConfig{}
	if err := cfg.Apply(scoutConfigRows()); err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.LiveTurnLimits(), (domain.LiveTurnLimits{PageSize: 50, MaxPageSize: 200}); got != want {
		t.Fatalf("LiveTurnLimits() = %+v, want the seeded defaults %+v", got, want)
	}
	zero := scoutConfigRows()
	for _, flag := range []string{agent_live_turn_page_size, agent_live_turn_max_page_size} {
		zero[flag] = keelconfig.ConfigRow{Value: "0"}
	}
	if err := (&ScoutConfig{}).Apply(zero); err != nil {
		t.Fatalf("zero takes the default: %v", err)
	}
	for flag, value := range map[string]string{
		agent_live_turn_page_size: "-1", agent_live_turn_max_page_size: "1001",
	} {
		rows := scoutConfigRows()
		rows[flag] = keelconfig.ConfigRow{Value: value}
		if err := (&ScoutConfig{}).Apply(rows); err == nil || !strings.Contains(err.Error(), flag) {
			t.Fatalf("%s=%s: Apply error = %v, want a refusal naming the flag", flag, value, err)
		}
	}
	// A default page larger than the largest page is refused, naming the default.
	rows := scoutConfigRows()
	rows[agent_live_turn_page_size] = keelconfig.ConfigRow{Value: "300"}
	if err := (&ScoutConfig{}).Apply(rows); err == nil || !strings.Contains(err.Error(), agent_live_turn_page_size) {
		t.Fatalf("page above the maximum: %v", err)
	}
}
