package fake

import (
	"context"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

type GovernanceSource struct {
	SnapshotFunc func(context.Context, int64, string) (domain.GovernanceSnapshot, bool, error)
}

func (f GovernanceSource) Snapshot(ctx context.Context, tenantID int64, agentID string) (domain.GovernanceSnapshot, bool, error) {
	return f.SnapshotFunc(ctx, tenantID, agentID)
}

type TriggerBindingVerifier struct {
	VerifyFunc func(context.Context, int64, string, domain.AgentTrigger) (bool, error)
}

func (f TriggerBindingVerifier) Verify(ctx context.Context, tenantID int64, agentID string, trigger domain.AgentTrigger) (bool, error) {
	return f.VerifyFunc(ctx, tenantID, agentID, trigger)
}

var (
	_ contract.GovernanceSource       = GovernanceSource{}
	_ contract.TriggerBindingVerifier = TriggerBindingVerifier{}
)
