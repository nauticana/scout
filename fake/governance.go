package fake

import (
	"context"
	"fmt"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

type GovernanceSource struct {
	SnapshotFunc func(ctx context.Context, tenantID int64, agentID, version string) (domain.GovernanceSnapshot, bool, error)
}

func (f GovernanceSource) Snapshot(ctx context.Context, tenantID int64, agentID, version string) (domain.GovernanceSnapshot, bool, error) {
	return f.SnapshotFunc(ctx, tenantID, agentID, version)
}

type TriggerBindingVerifier struct {
	VerifyFunc func(context.Context, int64, string, domain.AgentTrigger) (bool, error)
}

func (f TriggerBindingVerifier) Verify(ctx context.Context, tenantID int64, agentID string, trigger domain.AgentTrigger) (bool, error) {
	return f.VerifyFunc(ctx, tenantID, agentID, trigger)
}

// CharterObjects stores each document's kind and identifier under its object id.
type CharterObjects map[int64][2]string

func (f CharterObjects) ObjectIDs(_ context.Context, _ int64, kind string, identifiers ...string) ([]int64, error) {
	ids := make([]int64, len(identifiers))
next:
	for i, identifier := range identifiers {
		for id, document := range f {
			if document == [2]string{kind, identifier} {
				ids[i] = id
				continue next
			}
		}
		return nil, fmt.Errorf("%w: %s %s", domain.ErrNotFound, kind, identifier)
	}
	return ids, nil
}

func (f CharterObjects) Identifiers(_ context.Context, _ int64, ids ...int64) ([]string, error) {
	identifiers := make([]string, len(ids))
	for i, id := range ids {
		document, ok := f[id]
		if !ok {
			return nil, fmt.Errorf("%w: object %d", domain.ErrNotFound, id)
		}
		identifiers[i] = document[1]
	}
	return identifiers, nil
}

var (
	_ contract.GovernanceSource       = GovernanceSource{}
	_ contract.TriggerBindingVerifier = TriggerBindingVerifier{}
	_ contract.CharterObjects         = CharterObjects{}
)
