package charter

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nauticana/charter/sdk/corpus"
	"github.com/nauticana/charter/sdk/evidence"
	"github.com/nauticana/charter/sdk/model"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/workingset"
)

type factSource []domain.WorkingSetCandidate

func (source factSource) Candidates(context.Context, domain.WorkingSetRequest) (domain.WorkingSetCandidates, error) {
	return domain.WorkingSetCandidates{RequiredKeys: []string{"limit"}, Candidates: source}, nil
}

// judgedFacts runs Charter's CurrentFacts over the given facts of one order's credit limit.
func judgedFacts(t *testing.T, at time.Time, authority *model.FactAuthority, facts ...[3]string) []evidence.FactState {
	t.Helper()
	c := corpus.New()
	for _, f := range facts {
		raw := fmt.Sprintf(`{"charterSpecVersion":"1.6.0","namespace":"t","kind":"EvidenceRecord","id":%q,"category":"observed-fact","content":"c","recordedAt":%q,`+
			`"fact":{"subject":{"kind":"Customer","id":"C-1","external":true},"informationId":"CREDIT","attribute":"limit","value":%s,"validAt":%q,"systemProfileId":%q}}`,
			f[0], at.Add(-time.Hour).Format(time.RFC3339), f[2], at.Add(-time.Hour).Format(time.RFC3339), f[1])
		document, err := (corpus.Parser{}).Parse([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Add(document); err != nil {
			t.Fatal(err)
		}
	}
	definition := model.InformationDefinition{Envelope: model.Envelope{Namespace: "t", ID: "CREDIT"}, FactAuthority: authority}
	states, err := (evidence.Queries{Provider: evidence.NewBaseProvider(c)}).CurrentFacts(context.Background(), "t",
		model.ObjectRef{Kind: "Customer", ID: "C-1", External: true}, definition, at)
	if err != nil {
		t.Fatal(err)
	}
	return states
}

func compileFacts(t *testing.T, states []evidence.FactState) domain.WorkingSet {
	t.Helper()
	candidates, err := FactCandidates(states, func(content string) int64 { return int64(len(content)) })
	if err != nil {
		t.Fatal(err)
	}
	set, err := (&workingset.Compiler{Source: factSource(candidates)}).Compile(context.Background(), domain.WorkingSetRequest{
		TenantContext: domain.TenantContext{TenantID: 7}, Principal: domain.Principal{TenantID: 7}, RequestID: "request-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// Charter's verdict decides the working set: a ranked source is current, equal-rank disagreement
// is a conflict with both sides kept, and a fact past maxAge leaves the required limit unsupported.
func TestFactCandidatesKeepCharterVerdicts(t *testing.T) {
	at := time.Date(2026, 6, 18, 18, 0, 0, 0, time.UTC)
	ranked := &model.FactAuthority{Ranks: []model.FactSource{{SystemProfileID: &model.Ref{ID: "ERP"}}}}
	set := compileFacts(t, judgedFacts(t, at, ranked, [3]string{"ERP-1", "ERP", "5000"}, [3]string{"CRM-1", "CRM", "7000"}))
	if set.Assurance != domain.AssuranceVerified || len(set.Selected) != 1 || set.Selected[0].ID != "t/ERP-1" || fmt.Sprint(set.Excluded) != "[t/CRM-1]" {
		t.Fatalf("ranked: %+v", set)
	}
	set = compileFacts(t, judgedFacts(t, at, nil, [3]string{"ERP-1", "ERP", "5000"}, [3]string{"CRM-1", "CRM", "7000"}))
	if set.Assurance != domain.AssuranceConflicted || len(set.Selected) != 2 {
		t.Fatalf("unranked: %+v", set)
	}
	stale := &model.FactAuthority{Ranks: ranked.Ranks, MaxAgeSeconds: 60}
	set = compileFacts(t, judgedFacts(t, at, stale, [3]string{"ERP-1", "ERP", "5000"}))
	if set.Assurance != domain.AssuranceInsufficient || len(set.Selected) != 0 {
		t.Fatalf("stale: %+v", set)
	}
}

// A state Charter never returns, a record without a fact, or no token estimator fails closed.
func TestFactCandidatesRefuseUnjudgedRecords(t *testing.T) {
	tokens := func(string) int64 { return 1 }
	if _, err := FactCandidates([]evidence.FactState{{State: "fresh", Record: model.EvidenceRecord{Fact: &model.Fact{}}}}, tokens); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unknown state: %v", err)
	}
	if _, err := FactCandidates([]evidence.FactState{{State: evidence.FactCurrent}}, tokens); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("no fact: %v", err)
	}
	if _, err := FactCandidates(nil, nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("no estimator: %v", err)
	}
}
