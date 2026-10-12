package scout

import (
	"strings"
	"testing"
	"time"

	keelconfig "github.com/nauticana/keel/config"

	"github.com/nauticana/scout/domain"
)

// The operation flags reach dataplane.TableTurnControl and its lapse worker; zero takes each
// default, and a value outside its range is refused naming the flag.
func TestOperatorFlagsTakeTheirDefaultsAndRefuseOutOfRange(t *testing.T) {
	cfg := &ScoutConfig{}
	if err := cfg.Apply(scoutConfigRows()); err != nil {
		t.Fatal(err)
	}
	want := domain.OperatorSettings{Lease: 900 * time.Second, EntryMaxBytes: 16384, MaxEntries: 64, LapseBatch: 100, LapsePeriod: 30 * time.Second}
	if got := cfg.OperatorSettings(); got != want {
		t.Fatalf("OperatorSettings() = %+v, want the seeded defaults %+v", got, want)
	}
	zero := scoutConfigRows()
	for _, flag := range []string{agent_takeover_lease, agent_operator_entry_max_bytes, agent_operator_max_entries, agent_hold_lapse_batch, agent_hold_lapse_period} {
		zero[flag] = keelconfig.ConfigRow{Value: "0"}
	}
	if err := (&ScoutConfig{}).Apply(zero); err != nil {
		t.Fatalf("zero takes the default: %v", err)
	}
	for flag, value := range map[string]string{
		agent_takeover_lease: "86401", agent_operator_entry_max_bytes: "1048577", agent_operator_max_entries: "4097",
		agent_hold_lapse_batch: "10001", agent_hold_lapse_period: "3601", "agent_takeover_lease ": "-1",
	} {
		flag = strings.TrimSpace(flag)
		rows := scoutConfigRows()
		rows[flag] = keelconfig.ConfigRow{Value: value}
		if err := (&ScoutConfig{}).Apply(rows); err == nil || !strings.Contains(err.Error(), flag) {
			t.Fatalf("%s=%s: Apply error = %v, want a refusal naming the flag", flag, value, err)
		}
	}
}
