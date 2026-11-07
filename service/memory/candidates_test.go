package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

type sectionsFake struct {
	contract.MemoryStore
	sections []domain.MemorySection
	err      error
}

func (f sectionsFake) Sections(context.Context, int64) ([]domain.MemorySection, error) {
	return f.sections, f.err
}

func TestMemoryCandidatesOfferEachSectionForTheBudgetToDecide(t *testing.T) {
	source := &Candidates{Store: sectionsFake{sections: []domain.MemorySection{{SectionID: "style", Caption: "Style", Body: "terse"}}}}
	offered, err := source.Candidates(context.Background(), domain.WorkingSetRequest{TenantContext: domain.TenantContext{TenantID: 7}})
	if err != nil || len(offered.Candidates) != 1 {
		t.Fatalf("offered = %+v, %v", offered, err)
	}
	candidate := offered.Candidates[0]
	if candidate.ID != "memory:style" || candidate.Content != "terse" || candidate.Required || candidate.Standing != domain.StandingCurrent || candidate.TokenCost != 2 {
		t.Fatalf("candidate = %+v", candidate)
	}
	if _, err = (&Candidates{}).Candidates(context.Background(), domain.WorkingSetRequest{}); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("no store = %v", err)
	}
}
