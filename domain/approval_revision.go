package domain

// ProposalRevision is the edit a decider made to a proposal: who edited it, with which
// authority, and where the revised arguments are archived.
type ProposalRevision struct {
	Original  ApprovalKey
	Editor    PrincipalRef
	Authority AuthorityRef
	Reason    string
	Arguments ObjectRef
}

// CallRevision substitutes a decider's edited arguments for a proposed tool call. The loop
// journals it, so a replay makes the same substitution without asking again.
type CallRevision struct {
	// RevisesProposal is the approval step of the edited proposal.
	RevisesProposal int64  `json:"revises_proposal"`
	Arguments       []byte `json:"arguments"`
	ArgumentsDigest string `json:"arguments_digest"`
}
