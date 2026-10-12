package scout

import (
	"strings"
	"testing"
	"time"

	keelconfig "github.com/nauticana/keel/config"
)

// agent_outcome_attest_grace reaches the data plane in seconds; zero takes the 60-second default
// downstream, and a value outside 0..86400 is refused naming the flag.
func TestOutcomeAttestGraceTakesItsDefaultAndRefusesOutOfRange(t *testing.T) {
	cfg := &ScoutConfig{}
	if err := cfg.Apply(scoutConfigRows()); err != nil {
		t.Fatal(err)
	}
	if got := cfg.DataPlaneSettings().OutcomeAttestGrace; got != time.Minute {
		t.Fatalf("OutcomeAttestGrace = %s, want the seeded 60s", got)
	}
	zero := scoutConfigRows()
	zero[agent_outcome_attest_grace] = keelconfig.ConfigRow{Value: "0"}
	if err := (&ScoutConfig{}).Apply(zero); err != nil {
		t.Fatalf("zero takes the default: %v", err)
	}
	for _, value := range []string{"-1", "86401"} {
		rows := scoutConfigRows()
		rows[agent_outcome_attest_grace] = keelconfig.ConfigRow{Value: value}
		if err := (&ScoutConfig{}).Apply(rows); err == nil || !strings.Contains(err.Error(), agent_outcome_attest_grace) {
			t.Fatalf("%s: Apply error = %v, want a refusal naming the flag", value, err)
		}
	}
}
