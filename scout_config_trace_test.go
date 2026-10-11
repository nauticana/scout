package scout

import (
	"strings"
	"testing"

	keelconfig "github.com/nauticana/keel/config"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/dataplane"
)

func TestRunTraceFlagsTakeTheirDefaultsAndRefuseOutOfRange(t *testing.T) {
	cfg := &ScoutConfig{}
	if err := cfg.Apply(scoutConfigRows()); err != nil {
		t.Fatal(err)
	}
	want := domain.RunTraceLimits{MaxEvents: dataplane.DefaultRunTraceEvents, MaxJournalEntries: dataplane.DefaultRunTraceJournalEntries,
		MaxJournalBytes: dataplane.DefaultRunTraceJournalBytes}
	if got := cfg.RunTraceLimits(); got != want {
		t.Fatalf("RunTraceLimits() = %+v, want the seeded defaults %+v", got, want)
	}
	zero := scoutConfigRows()
	for _, flag := range []string{agent_run_trace_max_events, agent_run_trace_max_journal_entries, agent_run_trace_max_journal_bytes} {
		zero[flag] = keelconfig.ConfigRow{Value: "0"}
	}
	if err := (&ScoutConfig{}).Apply(zero); err != nil {
		t.Fatalf("zero takes the default: %v", err)
	}
	for flag, value := range map[string]string{
		agent_run_trace_max_events: "5001", agent_run_trace_max_journal_entries: "-1",
		agent_run_trace_max_journal_bytes: "1023",
	} {
		rows := scoutConfigRows()
		rows[flag] = keelconfig.ConfigRow{Value: value}
		if err := (&ScoutConfig{}).Apply(rows); err == nil || !strings.Contains(err.Error(), flag) {
			t.Fatalf("%s=%s: Apply error = %v, want a refusal naming the flag", flag, value, err)
		}
	}
}
