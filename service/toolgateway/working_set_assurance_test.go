package toolgateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

func assuranceGateway(sent *int, readOnly bool) (*GovernedGateway, *[]domain.DecisionRecord) {
	var calls []string
	gateway := governedGateway(&calls, fake.ToolTransportFunc(func(context.Context, domain.ToolCall, domain.ToolDefinition, []byte, time.Duration) (domain.ToolResult, error) {
		*sent++
		return domain.ToolResult{Output: []byte("ok")}, nil
	}))
	gateway.Registry = &fake.ToolRegistry{GetFunc: func(context.Context, int64, string, string) (domain.ToolDefinition, error) {
		return domain.ToolDefinition{ToolID: "search", Version: "v1", Endpoint: "https://example.invalid/tool", ReadOnly: readOnly}, nil
	}}
	records := &[]domain.DecisionRecord{}
	gateway.Audit = &fake.AuditSink{RecordFunc: func(_ context.Context, record domain.DecisionRecord) error {
		*records = append(*records, record)
		return nil
	}}
	return gateway, records
}

// A mutating call of a turn whose working set is conflicted, insufficient, or unverified is
// refused before it leaves, and the refusal is recorded like any other denial.
func TestGovernedGatewayDeniesAMutationOnAnUnassuredWorkingSet(t *testing.T) {
	for _, assurance := range []domain.WorkingSetAssurance{domain.AssuranceConflicted, domain.AssuranceInsufficient, domain.AssuranceUnverified} {
		sent := 0
		gateway, records := assuranceGateway(&sent, false)
		call := validToolCall()
		call.Assurance = assurance
		_, err := gateway.Invoke(context.Background(), call)
		if !errors.Is(err, domain.ErrForbidden) || !errors.Is(err, domain.ErrWorkingSetUnassured) || !errors.Is(err, domain.ErrNotSent) || sent != 0 {
			t.Fatalf("%s: error = %v, sent = %d", assurance, err, sent)
		}
		if len(*records) != 1 || (*records)[0].Outcome != domain.DecisionDeny || (*records)[0].Reason != "unassured" || (*records)[0].Category != domain.DecisionCategoryToolInvoke {
			t.Fatalf("%s: records = %+v", assurance, *records)
		}
	}
}

// Verified and bounded sets act; any set reads; and a turn without a working set behaves as before.
func TestGovernedGatewayAllowsWhatTheWorkingSetSupports(t *testing.T) {
	cases := []struct {
		assurance domain.WorkingSetAssurance
		readOnly  bool
	}{
		{domain.AssuranceVerified, false}, {domain.AssuranceBounded, false}, {"", false},
		{domain.AssuranceConflicted, true}, {domain.AssuranceUnverified, true},
	}
	for _, c := range cases {
		sent := 0
		gateway, _ := assuranceGateway(&sent, c.readOnly)
		call := validToolCall()
		call.Assurance = c.assurance
		if _, err := gateway.Invoke(context.Background(), call); err != nil || sent != 1 {
			t.Fatalf("%q read-only %v: error = %v, sent = %d", c.assurance, c.readOnly, err, sent)
		}
	}
}

// An assurance that is not a working_set_assurance code fails closed.
func TestGovernedGatewayRefusesAnUnknownAssurance(t *testing.T) {
	sent := 0
	gateway, _ := assuranceGateway(&sent, true)
	call := validToolCall()
	call.Assurance = "v"
	if _, err := gateway.Invoke(context.Background(), call); !errors.Is(err, domain.ErrValidation) || sent != 0 {
		t.Fatalf("error = %v, sent = %d", err, sent)
	}
}
