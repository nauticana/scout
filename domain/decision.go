package domain

import (
	"encoding/json"
	"time"
)

// DecisionOutcome is the bounded result class of one governed decision.
type DecisionOutcome string

const (
	DecisionAllow   DecisionOutcome = "allow"
	DecisionDeny    DecisionOutcome = "deny"
	DecisionPending DecisionOutcome = "pending"
)

// ObligationKind names a condition a policy attaches to an allow decision. An
// enforcement point that does not recognize a kind must fail, never proceed.
type ObligationKind string

const (
	ObligationRequireApproval ObligationKind = "require_approval"
	ObligationRedact          ObligationKind = "redact"
	ObligationCapSpend        ObligationKind = "cap_spend"
	ObligationRecordEvidence  ObligationKind = "record_evidence"
	ObligationNotify          ObligationKind = "notify"
)

// Obligation is one condition carried by a decision; Params is kind-specific JSON.
type Obligation struct {
	Kind   ObligationKind  `json:"kind"`
	Params json.RawMessage `json:"params,omitempty"`
}

// DecisionSubject is what a policy is asked about.
type DecisionSubject struct {
	Principal      Principal
	Action         string
	Resource       string
	ResourceKind   ResourceKind
	RequestID      string
	ConversationID string
	// Environment carries request-time facts a policy may read, as canonical JSON.
	Environment []byte
}

// Decision is one policy evaluation result. Reason is auditable text, not a log line.
type Decision struct {
	Outcome       DecisionOutcome
	Obligations   []Obligation
	PolicyID      string
	PolicyVersion string
	Reason        string
	EvaluatedAt   time.Time
}

// DecisionRecord is the durable evidence of one governed decision. It carries
// identity, authority, and reasoning; Evidence points at redacted content in
// object storage and never holds the content itself.
type DecisionRecord struct {
	// TenantID is zero only for platform-wide decisions such as global rollouts.
	TenantID       int64
	Principal      PrincipalRef
	Authority      AuthorityRef
	ScopeID        string
	Category       string
	Action         string
	Resource       string
	ReleaseVersion string
	PolicyID       string
	PolicyVersion  string
	Outcome        DecisionOutcome
	Obligations    []ObligationKind
	Reason         string
	// Payload is redacted evidence the sink stores; it never holds secrets or
	// inspected content. Evidence is the reference the sink resolved it to.
	Payload        []byte
	Evidence       ObjectRef
	RequestID      string
	ConversationID string
	OccurredAt     time.Time
}

// Decision categories name the governed boundary that produced a record.
const (
	DecisionCategoryModelRoute  = "model_route"
	DecisionCategoryToolInvoke  = "tool_invoke"
	DecisionCategoryRetrieval   = "retrieval"
	DecisionCategoryGuardrail   = "guardrail"
	DecisionCategoryApproval    = "approval"
	DecisionCategoryPublication = "publication"
	DecisionCategoryCredential  = "credential"
	DecisionCategoryStateChange = "state_change"
)

// DecisionQuery selects records for a timeline or an explain view. A positive
// TenantID selects one tenant; zero selects platform-wide records only.
type DecisionQuery struct {
	TenantID       int64
	Principal      *PrincipalRef
	Category       string
	Resource       string
	RequestID      string
	ConversationID string
	Outcome        DecisionOutcome
	Since          time.Time
	Until          time.Time
	// Before pages backwards from a record id; zero starts at the newest.
	Before int64
	Limit  int
}

// DecisionPage is one page of records, newest first.
type DecisionPage struct {
	Records []DecisionRecord
	// NextBefore continues the page; zero means the query is exhausted.
	NextBefore int64
}
