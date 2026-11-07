package memory

import (
	"cmp"
	"context"
	"fmt"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/workingset"
)

// memoryAuthorityRank places what agents learned below the knowledge a release binds.
const memoryAuthorityRank = 2

// Candidates offers the tenant's memory to a working set, each section a candidate the budget may
// leave out and the turn records. Compose it instead of ReleaseLoopRequestBuilder.Memory.
type Candidates struct {
	Store contract.MemoryStore
}

var _ contract.WorkingSetCandidateSource = (*Candidates)(nil)

func (source *Candidates) Candidates(ctx context.Context, request domain.WorkingSetRequest) (domain.WorkingSetCandidates, error) {
	if source.Store == nil {
		return domain.WorkingSetCandidates{}, fmt.Errorf("%w: memory candidates need a memory store", domain.ErrNotReady)
	}
	sections, err := source.Store.Sections(ctx, request.TenantContext.TenantID)
	if err != nil {
		return domain.WorkingSetCandidates{}, err
	}
	var offered domain.WorkingSetCandidates
	for _, section := range sections {
		offered.Candidates = append(offered.Candidates, domain.WorkingSetCandidate{
			ID: "memory:" + section.SectionID, Content: section.Body, Provenance: "tenant memory: " + cmp.Or(section.Caption, section.SectionID),
			AuthorityRank: memoryAuthorityRank, Standing: domain.StandingCurrent, ReliabilityPercent: domain.MaxWorkingSetReliability,
			TokenCost: workingset.EstimateTokens(section.Body),
		})
	}
	return offered, nil
}
