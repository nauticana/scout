package domain

// WorkingSetAssurance is how far a turn's working set can be relied on, after EAF section 4.7:
// a working_set_assurance code.
type WorkingSetAssurance string

const (
	// AssuranceVerified keeps every required item and controlling authority current and nothing current was left out.
	AssuranceVerified WorkingSetAssurance = "V"
	// AssuranceBounded is verified, but the budget left out current optional context.
	AssuranceBounded WorkingSetAssurance = "B"
	// AssuranceConflicted keeps both sides of a blocking conflict, which only a person settles.
	AssuranceConflicted WorkingSetAssurance = "C"
	// AssuranceInsufficient lacks a current candidate for a required item.
	AssuranceInsufficient WorkingSetAssurance = "I"
	// AssuranceUnverified declares nothing to verify against.
	AssuranceUnverified WorkingSetAssurance = "U"
)

// Valid reports whether the assurance is a seeded working_set_assurance code.
func (assurance WorkingSetAssurance) Valid() bool {
	switch assurance {
	case AssuranceVerified, AssuranceBounded, AssuranceConflicted, AssuranceInsufficient, AssuranceUnverified:
		return true
	}
	return false
}

// Acts reports whether the turn may take a mutating action on this working set.
func (assurance WorkingSetAssurance) Acts() bool {
	return assurance == AssuranceVerified || assurance == AssuranceBounded
}

// Unsettled reports a working set the model must not reason over: a person settles it first.
func (assurance WorkingSetAssurance) Unsettled() bool {
	return assurance == AssuranceConflicted || assurance == AssuranceInsufficient
}

// CandidateStanding is a candidate's temporal and authority standing at the turn, as its source judged it.
type CandidateStanding string

const (
	StandingCurrent    CandidateStanding = "current"
	StandingStale      CandidateStanding = "stale"
	StandingSuperseded CandidateStanding = "superseded"
	// StandingOutranked is current but stated by a lower-ranked source than another current one.
	StandingOutranked CandidateStanding = "outranked"
	// StandingConflicted is one side of a blocking conflict: sources of equal rank disagree.
	StandingConflicted CandidateStanding = "conflicted"
)

// Valid reports whether the standing is one of the known standings.
func (standing CandidateStanding) Valid() bool {
	switch standing {
	case StandingCurrent, StandingStale, StandingSuperseded, StandingOutranked, StandingConflicted:
		return true
	}
	return false
}

// MaxWorkingSetCandidateIDBytes is the width of turn_working_set_item.candidate_id.
const MaxWorkingSetCandidateIDBytes = 200

// MaxWorkingSetReliability is the reliability of a fully reliable candidate, in percent.
const MaxWorkingSetReliability = 100

// WorkingSetCandidate is one item of context a turn may reason over.
type WorkingSetCandidate struct {
	ID string
	// Key is what the candidate states, such as a fact's attribute; empty means its ID. A required
	// key is satisfied by a current candidate stating it.
	Key string
	// Content is the text the model reads; Reference points at it instead. At least one is set.
	Content    string
	Reference  *ObjectRef
	Provenance string
	// AuthorityRank orders sources, 0 the most authoritative.
	AuthorityRank      int
	Standing           CandidateStanding
	ReliabilityPercent int
	// Required and Controlling candidates make their key required and are never dropped for budget.
	Required    bool
	Controlling bool
	TokenCost   int64
}

// WorkingSetCandidates is what a source offers one turn: its candidates and the keys the turn needs.
type WorkingSetCandidates struct {
	RequiredKeys []string
	Candidates   []WorkingSetCandidate
}

// WorkingSetRequest names the turn a working set is compiled for.
type WorkingSetRequest struct {
	TenantContext  TenantContext
	Principal      Principal
	RequestID      string
	ConversationID string
	Task           string
}

// WorkingSet is the smallest model-active context that keeps the required evidence and the
// controlling authority, with its assurance and the candidates it left out.
type WorkingSet struct {
	Assurance WorkingSetAssurance
	Selected  []WorkingSetCandidate
	Excluded  []string
	TokenCost int64
}

// WorkingSetLimits bound a compilation; zero takes the compiler's default.
type WorkingSetLimits struct {
	MaxCandidates int
	MaxTokens     int64
}
