package domain

import (
	"slices"
	"strings"
	"testing"
)

// §2: AGENT_OPERATION seeds exactly the Go actions; an administrator holds all four, an operator
// views and operates but neither preempts nor attests.
func TestOperationActionsAgreeWithTheirSeedAndGrants(t *testing.T) {
	actions := map[string][]string{}
	var seeded []string
	for _, line := range readSchemaFile(t, "seed/runtime/runtime.yml") {
		row, ok := strings.CutPrefix(strings.TrimSpace(line), "- [")
		if !ok {
			continue
		}
		fields := strings.Split(strings.TrimSuffix(row, "]"), ",")
		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i])
		}
		switch {
		case len(fields) == 3 && fields[0] == OperationAuthorizationObject:
			seeded = append(seeded, fields[1])
		case len(fields) == 4 && fields[1] == OperationAuthorizationObject:
			actions[fields[0]] = append(actions[fields[0]], fields[2])
		}
	}
	want := codes(OperationView, OperationOperate, OperationPreempt, OperationReconcile)
	if !slices.Equal(sortedCopy(seeded), sortedCopy(want)) {
		t.Fatalf("seeded actions %v, Go actions %v", seeded, want)
	}
	if got := sortedCopy(actions["AGENT_ADMIN"]); !slices.Equal(got, sortedCopy(want)) {
		t.Fatalf("AGENT_ADMIN holds %v, want every action", got)
	}
	if got := sortedCopy(actions["AGENT_OPER"]); !slices.Equal(got, sortedCopy(codes(OperationView, OperationOperate))) {
		t.Fatalf("AGENT_OPER holds %v, want VIEW and OPERATE only", got)
	}
}
