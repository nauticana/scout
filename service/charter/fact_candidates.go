package charter

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nauticana/charter/sdk/evidence"

	"github.com/nauticana/scout/domain"
)

// factStandings maps Charter's standing of a fact onto a working set candidate's.
var factStandings = map[string]domain.CandidateStanding{
	evidence.FactCurrent:    domain.StandingCurrent,
	evidence.FactStale:      domain.StandingStale,
	evidence.FactSuperseded: domain.StandingSuperseded,
	evidence.FactOutranked:  domain.StandingOutranked,
	evidence.FactConflicted: domain.StandingConflicted,
}

// FactCandidates turns one evidence.Queries.CurrentFacts judgement into working set candidates
// keyed by attribute, so a product's source names required attributes as required keys. Charter
// decides freshness, supersession, rank, and conflict; Scout keeps its verdict and never re-ranks,
// so every candidate has rank 0. tokens prices a candidate's content and is required.
func FactCandidates(states []evidence.FactState, tokens func(content string) int64) ([]domain.WorkingSetCandidate, error) {
	if tokens == nil {
		return nil, fmt.Errorf("%w: fact candidates need a token estimator", domain.ErrValidation)
	}
	candidates := make([]domain.WorkingSetCandidate, 0, len(states))
	for _, state := range states {
		record := state.Record
		standing, known := factStandings[state.State]
		if !known || record.Fact == nil {
			return nil, fmt.Errorf("%w: record %s/%s is not a judged fact (state %q)", domain.ErrValidation, record.Namespace, record.ID, state.State)
		}
		fact := record.Fact
		content, err := json.Marshal(struct {
			Attribute string    `json:"attribute"`
			Value     any       `json:"value"`
			ValidAt   time.Time `json:"validAt"`
		}{fact.Attribute, fact.Value, fact.ValidAt})
		if err != nil {
			return nil, fmt.Errorf("%w: fact %s/%s: %w", domain.ErrValidation, record.Namespace, record.ID, err)
		}
		provenance := "charter evidence " + record.Category
		if fact.SystemProfileID != nil {
			provenance += " from " + strings.TrimPrefix(fact.SystemProfileID.Namespace+"/"+fact.SystemProfileID.ID, "/")
		}
		candidates = append(candidates, domain.WorkingSetCandidate{
			ID: record.Namespace + "/" + record.ID, Key: fact.Attribute, Content: string(content),
			Provenance: provenance + " (" + state.Rule + ")", Standing: standing, TokenCost: tokens(string(content)),
		})
	}
	return candidates, nil
}
