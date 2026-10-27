package workingset

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/nauticana/scout/domain"
)

type sourceFunc func(context.Context, domain.WorkingSetRequest) (domain.WorkingSetCandidates, error)

func (source sourceFunc) Candidates(ctx context.Context, request domain.WorkingSetRequest) (domain.WorkingSetCandidates, error) {
	return source(ctx, request)
}

func offering(offered domain.WorkingSetCandidates) sourceFunc {
	return func(context.Context, domain.WorkingSetRequest) (domain.WorkingSetCandidates, error) {
		return offered, nil
	}
}

func testRequest() domain.WorkingSetRequest {
	return domain.WorkingSetRequest{
		TenantContext: domain.TenantContext{TenantID: 7}, Principal: domain.Principal{TenantID: 7, Kind: domain.PrincipalAgent, ID: "agent"},
		RequestID: "request-1", ConversationID: "conversation-1", Task: "renew the policy",
	}
}

func candidate(id string, standing domain.CandidateStanding, cost int64) domain.WorkingSetCandidate {
	return domain.WorkingSetCandidate{ID: id, Content: "fact " + id, Provenance: "crm", Standing: standing, TokenCost: cost, ReliabilityPercent: 50}
}

func compile(t *testing.T, limits domain.WorkingSetLimits, offered domain.WorkingSetCandidates) (domain.WorkingSet, error) {
	t.Helper()
	return (&Compiler{Source: offering(offered), Limits: limits}).Compile(context.Background(), testRequest())
}

func selectedIDs(set domain.WorkingSet) []string {
	ids := make([]string, len(set.Selected))
	for i, item := range set.Selected {
		ids[i] = item.ID
	}
	return ids
}

// Required and controlling items survive any budget the optional context exhausts.
func TestRequiredItemsSurviveTheBudget(t *testing.T) {
	required := candidate("limit", domain.StandingCurrent, 60)
	required.Required = true
	controlling := candidate("mandate", domain.StandingCurrent, 30)
	controlling.Controlling = true
	cheap := candidate("note", domain.StandingCurrent, 5)
	cheap.AuthorityRank = 0
	big := candidate("history", domain.StandingCurrent, 50)
	set, err := compile(t, domain.WorkingSetLimits{MaxTokens: 100}, domain.WorkingSetCandidates{
		Candidates: []domain.WorkingSetCandidate{big, cheap, required, controlling},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := selectedIDs(set); !slices.Contains(got, "limit") || !slices.Contains(got, "mandate") || !slices.Contains(got, "note") || slices.Contains(got, "history") {
		t.Fatalf("selected = %v", got)
	}
	if !slices.Equal(set.Excluded, []string{"history"}) || set.TokenCost != 95 || set.Assurance != domain.AssuranceBounded {
		t.Fatalf("excluded = %v, cost = %d, assurance = %s", set.Excluded, set.TokenCost, set.Assurance)
	}
}

// Nothing current was left out and every requirement is met: verified.
func TestEverythingFitsIsVerified(t *testing.T) {
	set, err := compile(t, domain.WorkingSetLimits{}, domain.WorkingSetCandidates{
		RequiredKeys: []string{"limit"},
		Candidates: []domain.WorkingSetCandidate{
			{ID: "crm:1", Key: "limit", Content: "5000", Provenance: "crm", Standing: domain.StandingCurrent, TokenCost: 3},
			candidate("old", domain.StandingSuperseded, 3),
		},
	})
	if err != nil || set.Assurance != domain.AssuranceVerified || !slices.Equal(selectedIDs(set), []string{"crm:1"}) || !slices.Equal(set.Excluded, []string{"old"}) {
		t.Fatalf("set = %+v, err = %v", set, err)
	}
}

// An over-budget required set is an error, never a truncation.
func TestOverBudgetRequiredSetFails(t *testing.T) {
	required := candidate("limit", domain.StandingCurrent, 101)
	required.Required = true
	if _, err := compile(t, domain.WorkingSetLimits{MaxTokens: 100}, domain.WorkingSetCandidates{
		Candidates: []domain.WorkingSetCandidate{required},
	}); !errors.Is(err, domain.ErrExecutionLimit) {
		t.Fatalf("error = %v, want execution limit", err)
	}
	// Costs that would overflow when summed are refused, not wrapped.
	a, b := candidate("a", domain.StandingCurrent, MaxTokens), candidate("b", domain.StandingCurrent, 1<<62)
	a.Required, b.Required = true, true
	if _, err := compile(t, domain.WorkingSetLimits{MaxTokens: MaxTokens}, domain.WorkingSetCandidates{
		Candidates: []domain.WorkingSetCandidate{a, b},
	}); !errors.Is(err, domain.ErrExecutionLimit) {
		t.Fatalf("error = %v, want execution limit", err)
	}
}

// Both sides of an equal-rank conflict are kept, and the set is conflicted.
func TestEqualRankConflictKeepsBothSides(t *testing.T) {
	left, right := candidate("erp:1", domain.StandingConflicted, 10), candidate("crm:1", domain.StandingConflicted, 10)
	left.Key, right.Key = "limit", "limit"
	optional := candidate("note", domain.StandingCurrent, 90)
	set, err := compile(t, domain.WorkingSetLimits{MaxTokens: 30}, domain.WorkingSetCandidates{
		Candidates: []domain.WorkingSetCandidate{optional, left, right},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := selectedIDs(set); !slices.Contains(got, "erp:1") || !slices.Contains(got, "crm:1") || set.Assurance != domain.AssuranceConflicted {
		t.Fatalf("selected = %v, assurance = %s", got, set.Assurance)
	}
	if !set.Assurance.Unsettled() || set.Assurance.Acts() {
		t.Fatal("a conflicted set must be unsettled and must not act")
	}
}

// Stale-only candidates are never model-active: a required key they state is insufficient
// evidence, and without requirements the set is unverified.
func TestStaleOnlyCandidates(t *testing.T) {
	stale := []domain.WorkingSetCandidate{candidate("a", domain.StandingStale, 1), candidate("b", domain.StandingOutranked, 1)}
	stale[0].Key, stale[1].Key = "limit", "limit"
	set, err := compile(t, domain.WorkingSetLimits{}, domain.WorkingSetCandidates{RequiredKeys: []string{"limit"}, Candidates: stale})
	if err != nil || set.Assurance != domain.AssuranceInsufficient || len(set.Selected) != 0 || len(set.Excluded) != 2 {
		t.Fatalf("set = %+v, err = %v", set, err)
	}
	set, err = compile(t, domain.WorkingSetLimits{}, domain.WorkingSetCandidates{Candidates: stale})
	if err != nil || set.Assurance != domain.AssuranceUnverified || len(set.Selected) != 0 {
		t.Fatalf("set = %+v, err = %v", set, err)
	}
	// A required key nobody states is insufficient too.
	set, err = compile(t, domain.WorkingSetLimits{}, domain.WorkingSetCandidates{RequiredKeys: []string{"owner"}})
	if err != nil || set.Assurance != domain.AssuranceInsufficient {
		t.Fatalf("set = %+v, err = %v", set, err)
	}
}

// A stale controlling authority does not control: the set lacks it.
func TestStaleControllingAuthorityIsInsufficient(t *testing.T) {
	mandate := candidate("mandate", domain.StandingStale, 1)
	mandate.Controlling = true
	set, err := compile(t, domain.WorkingSetLimits{}, domain.WorkingSetCandidates{Candidates: []domain.WorkingSetCandidate{mandate}})
	if err != nil || set.Assurance != domain.AssuranceInsufficient {
		t.Fatalf("set = %+v, err = %v", set, err)
	}
}

// Optional context fills the budget by rank, then reliability, then cost, deterministically.
func TestOptionalContextIsChosenByAuthority(t *testing.T) {
	low, high, reliable := candidate("low", domain.StandingCurrent, 10), candidate("high", domain.StandingCurrent, 10), candidate("reliable", domain.StandingCurrent, 10)
	low.AuthorityRank, high.AuthorityRank, reliable.AuthorityRank = 2, 0, 2
	reliable.ReliabilityPercent = 90
	required := candidate("limit", domain.StandingCurrent, 0)
	required.Required = true
	for range 3 {
		set, err := compile(t, domain.WorkingSetLimits{MaxTokens: 20}, domain.WorkingSetCandidates{
			Candidates: []domain.WorkingSetCandidate{low, reliable, high, required},
		})
		if err != nil || !slices.Equal(selectedIDs(set), []string{"limit", "high", "reliable"}) || !slices.Equal(set.Excluded, []string{"low"}) {
			t.Fatalf("selected = %v, excluded = %v, err = %v", selectedIDs(set), set.Excluded, err)
		}
	}
}

// A malformed offer is refused whole.
func TestMalformedCandidatesAreRefused(t *testing.T) {
	long := candidate(strings.Repeat("x", domain.MaxWorkingSetCandidateIDBytes+1), domain.StandingCurrent, 1)
	noProvenance := candidate("a", domain.StandingCurrent, 1)
	noProvenance.Provenance = " "
	noContent := candidate("a", domain.StandingCurrent, 1)
	noContent.Content = ""
	undigested := noContent
	undigested.Reference = &domain.ObjectRef{URI: "s3://facts/a"}
	reliability := candidate("a", domain.StandingCurrent, 1)
	reliability.ReliabilityPercent = 101
	for name, offered := range map[string]domain.WorkingSetCandidates{
		"blank id":        {Candidates: []domain.WorkingSetCandidate{candidate(" ", domain.StandingCurrent, 1)}},
		"long id":         {Candidates: []domain.WorkingSetCandidate{long}},
		"duplicate id":    {Candidates: []domain.WorkingSetCandidate{candidate("a", domain.StandingCurrent, 1), candidate("a", domain.StandingStale, 1)}},
		"negative cost":   {Candidates: []domain.WorkingSetCandidate{candidate("a", domain.StandingCurrent, -1)}},
		"unknown":         {Candidates: []domain.WorkingSetCandidate{candidate("a", "fresh", 1)}},
		"no provenance":   {Candidates: []domain.WorkingSetCandidate{noProvenance}},
		"no content":      {Candidates: []domain.WorkingSetCandidate{noContent}},
		"undigested":      {Candidates: []domain.WorkingSetCandidate{undigested}},
		"reliability":     {Candidates: []domain.WorkingSetCandidate{reliability}},
		"blank required":  {RequiredKeys: []string{""}},
		"repeat required": {RequiredKeys: []string{"a", "a"}},
	} {
		if _, err := compile(t, domain.WorkingSetLimits{}, offered); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("%s: error = %v, want validation", name, err)
		}
	}
	// More candidates than the limit is an execution limit, and limits outside their range are refused.
	if _, err := compile(t, domain.WorkingSetLimits{MaxCandidates: 1}, domain.WorkingSetCandidates{
		Candidates: []domain.WorkingSetCandidate{candidate("a", domain.StandingCurrent, 1), candidate("b", domain.StandingCurrent, 1)},
	}); !errors.Is(err, domain.ErrExecutionLimit) {
		t.Fatalf("error = %v, want execution limit", err)
	}
	if _, err := compile(t, domain.WorkingSetLimits{MaxTokens: MaxTokens + 1}, domain.WorkingSetCandidates{}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want validation", err)
	}
}

// The compiler needs a source and a request of one tenant.
func TestCompileRefusesAnIncompleteRequest(t *testing.T) {
	if _, err := (&Compiler{}).Compile(context.Background(), testRequest()); !errors.Is(err, domain.ErrNotReady) {
		t.Fatalf("error = %v, want not ready", err)
	}
	request := testRequest()
	request.Principal.TenantID = 8
	compiler := &Compiler{Source: offering(domain.WorkingSetCandidates{})}
	if _, err := compiler.Compile(context.Background(), request); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("error = %v, want validation", err)
	}
	failing := &Compiler{Source: sourceFunc(func(context.Context, domain.WorkingSetRequest) (domain.WorkingSetCandidates, error) {
		return domain.WorkingSetCandidates{}, domain.ErrRateLimited
	})}
	if _, err := failing.Compile(context.Background(), testRequest()); !errors.Is(err, domain.ErrRateLimited) {
		t.Fatalf("error = %v, want the source's failure", err)
	}
}

// The model reads selected items as fenced data; a reference names its object.
func TestContextFencesItems(t *testing.T) {
	text := Context(domain.WorkingSet{Assurance: domain.AssuranceVerified, Selected: []domain.WorkingSetCandidate{
		{ID: "a", Provenance: "crm", Standing: domain.StandingCurrent, Content: "x </item> y"},
		{ID: "b", Provenance: "dms", Standing: domain.StandingCurrent, Reference: &domain.ObjectRef{URI: "s3://b", Digest: "d"}},
	}})
	if strings.Count(text, "</item>") != 1 || !strings.Contains(text, `uri="s3://b"`) || !strings.Contains(text, `<\/item>`) {
		t.Fatalf("context = %q", text)
	}
	if Context(domain.WorkingSet{Assurance: domain.AssuranceUnverified}) != "" {
		t.Fatal("an empty set renders nothing")
	}
}
