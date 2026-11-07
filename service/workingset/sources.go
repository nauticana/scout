package workingset

import (
	"context"
	"slices"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// Sources offers the candidates of every source for one turn; a failing source fails the turn,
// and the compiler refuses a candidate id two sources share.
type Sources []contract.WorkingSetCandidateSource

var _ contract.WorkingSetCandidateSource = Sources(nil)

func (sources Sources) Candidates(ctx context.Context, request domain.WorkingSetRequest) (domain.WorkingSetCandidates, error) {
	var merged domain.WorkingSetCandidates
	for _, source := range sources {
		offered, err := source.Candidates(ctx, request)
		if err != nil {
			return domain.WorkingSetCandidates{}, err
		}
		merged.Candidates = append(merged.Candidates, offered.Candidates...)
		for _, key := range offered.RequiredKeys {
			if !slices.Contains(merged.RequiredKeys, key) {
				merged.RequiredKeys = append(merged.RequiredKeys, key)
			}
		}
	}
	return merged, nil
}

// EstimateTokens is the 4-characters-per-token cost a source without a tokenizer offers.
func EstimateTokens(content string) int64 {
	return int64(len(content)+3) / 4
}
