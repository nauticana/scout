package contract

import (
	"context"

	"github.com/nauticana/scout/domain"
)

// WorkingSetCompiler selects the smallest context a turn may reason over that keeps its required
// evidence and controlling authority, and states how far that context can be relied on. Required
// and controlling items are never dropped for budget: when they do not fit, Compile fails.
type WorkingSetCompiler interface {
	Compile(ctx context.Context, request domain.WorkingSetRequest) (domain.WorkingSet, error)
}

// WorkingSetCandidateSource offers a turn's candidates with their provenance, authority, and
// standing as the source judged them; a product supplies it.
type WorkingSetCandidateSource interface {
	Candidates(ctx context.Context, request domain.WorkingSetRequest) (domain.WorkingSetCandidates, error)
}

// WorkingSetRecorder keeps a turn's working set assurance and the candidates it selected and
// left out, as the owner of the turn.
type WorkingSetRecorder interface {
	RecordWorkingSet(ctx context.Context, owner domain.TurnOwner, set domain.WorkingSet) error
}
