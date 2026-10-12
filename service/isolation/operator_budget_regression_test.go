package isolation

import (
	"strings"
	"testing"
)

// The turn's own hold is the agent's latest attempt: a person's hold of the same request, which
// carries the human principal kind, is never replayed or renumbered as the turn's.
func TestTheTurnsHoldLookupSkipsAPersonsHolds(t *testing.T) {
	if sql := budgetQueries[qFindBudget]; !strings.Contains(sql, "principal_kind = 'agent'") {
		t.Fatalf("the turn's hold lookup reads every principal's attempts:\n%s", sql)
	}
}
