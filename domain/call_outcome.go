package domain

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// CallOutcomeStatus is where one dispatched, non-read-only tool call stands: a
// turn_call_outcome_status code.
type CallOutcomeStatus string

const (
	// CallInFlight is the intent recorded before the call left; it holds until the call settles.
	CallInFlight CallOutcomeStatus = "I"
	// CallAccepted is a call the tool took asynchronously with a validated ToolAcceptance.
	CallAccepted  CallOutcomeStatus = "A"
	CallCompleted CallOutcomeStatus = "C"
	CallFailed    CallOutcomeStatus = "F"
	// CallUnknown is a dispatched call whose effect nobody could establish; it must be reconciled.
	CallUnknown CallOutcomeStatus = "U"
)

// Valid reports whether the status is a seeded turn_call_outcome_status code.
func (status CallOutcomeStatus) Valid() bool {
	switch status {
	case CallInFlight, CallAccepted, CallCompleted, CallFailed, CallUnknown:
		return true
	}
	return false
}

// Open reports whether the call still awaits a definite outcome; an open call holds its turn.
func (status CallOutcomeStatus) Open() bool {
	return status == CallInFlight || status == CallAccepted || status == CallUnknown
}

// Definite reports whether the status is a settled completed or failed outcome.
func (status CallOutcomeStatus) Definite() bool {
	return status == CallCompleted || status == CallFailed
}

// CallResolution names what settled a call: a turn_call_resolution code.
type CallResolution string

const (
	// ResolvedByGateway is the normal path: the gateway's answer to the owner's own call.
	ResolvedByGateway CallResolution = "G"
	// ResolvedByWorker is a late report by a fenced-out worker about its own dispatch.
	ResolvedByWorker CallResolution = "W"
	// ResolvedByEffect is an effect verifier's observation.
	ResolvedByEffect CallResolution = "E"
	// ResolvedByLookup is the composed ToolOutcomeLookup's answer.
	ResolvedByLookup CallResolution = "L"
	// ResolvedByAttestation is a person's attestation after the call's deadline.
	ResolvedByAttestation CallResolution = "T"
	// ResolvedByConfirmation is a linked MCP confirmation a person reconciled.
	ResolvedByConfirmation CallResolution = "M"
)

// Valid reports whether the code is a seeded turn_call_resolution code.
func (resolution CallResolution) Valid() bool {
	switch resolution {
	case ResolvedByGateway, ResolvedByWorker, ResolvedByEffect, ResolvedByLookup, ResolvedByAttestation, ResolvedByConfirmation:
		return true
	}
	return false
}

// CallOrigin is who made a call: a turn_call_origin code. Operator calls come with takeover.
type CallOrigin string

// CallFromAgent is a call the agent's tool loop made.
const CallFromAgent CallOrigin = "A"

// Valid reports whether the code is a seeded turn_call_origin code.
func (origin CallOrigin) Valid() bool { return origin == CallFromAgent }

// MaxAcceptanceWait bounds how long an accepted call may ask its lookup to be awaited.
const MaxAcceptanceWait = 7 * 24 * time.Hour

// maxAcceptanceHandle is the largest opaque handle, in bytes.
const maxAcceptanceHandle = 512

// ToolAcceptance is a provider-neutral asynchronous acceptance: the tool took the call and
// finishes it later. Handle is the opaque reference its lookup answers by, and LookupWithin
// how long until a lookup can be definite. MCPConfirmationID, when set, links a call held for
// a person's MCP confirmation, whose reconciliation settles the call.
type ToolAcceptance struct {
	Handle            string        `json:"handle"`
	LookupWithin      time.Duration `json:"lookup_within"`
	MCPConfirmationID int64         `json:"mcp_confirmation_id,omitempty"`
}

// Validate refuses an acceptance a lookup could not answer by.
func (acceptance ToolAcceptance) Validate() error {
	handle := acceptance.Handle
	switch {
	case strings.TrimSpace(handle) == "" || len(handle) > maxAcceptanceHandle || !utf8.ValidString(handle) ||
		strings.ContainsFunc(handle, unicode.IsControl):
		return fmt.Errorf("%w: an acceptance handle is 1 to %d printable bytes", ErrInvalidModelOutput, maxAcceptanceHandle)
	case acceptance.LookupWithin <= 0 || acceptance.LookupWithin > MaxAcceptanceWait:
		return fmt.Errorf("%w: an acceptance lookup deadline lies within (0, %s]", ErrInvalidModelOutput, MaxAcceptanceWait)
	case acceptance.MCPConfirmationID < 0:
		return fmt.Errorf("%w: an acceptance names a negative confirmation", ErrInvalidModelOutput)
	}
	return nil
}

// ToolOutcomeTraits is what a product's trusted composition declares about how one tool's
// outcomes can be recovered. Scout cannot verify either declaration, so neither is ever read
// from a registered tool or anything a tenant writes.
type ToolOutcomeTraits struct {
	// DedupesByKey declares the provider applies a call at most once per idempotency key, so a
	// dispatched call whose outcome is unknown may be sent again under the same key.
	DedupesByKey bool
	// Lookup declares the composed ToolOutcomeLookup answers the tool's calls by idempotency key,
	// so the tool may accept a call asynchronously with a ToolAcceptance.
	Lookup bool
}

// CallOutcomeKey identifies one call of one loop step of one turn.
type CallOutcomeKey struct {
	TenantID        int64
	RequestID       string
	ExecutionStepID int64
	CallID          string
}

// Validate refuses a key that names no call.
func (key CallOutcomeKey) Validate() error {
	if key.TenantID <= 0 || strings.TrimSpace(key.RequestID) == "" || key.ExecutionStepID <= 0 ||
		strings.TrimSpace(key.CallID) == "" || len(key.CallID) > 200 {
		return fmt.Errorf("%w: a call outcome needs a tenant, a request, an execution step, and a call id of at most 200 bytes", ErrValidation)
	}
	return nil
}

// CallIntent is what the tool loop records before a non-read-only call leaves. Window is how
// long the call may stay in flight: its deadline is the store clock plus Window.
type CallIntent struct {
	Key            CallOutcomeKey
	IdempotencyKey string
	Tool           ToolReference
	Window         time.Duration
}

// CallOutcome is the durable outcome record of one dispatched call. DeadlinePassed reports
// whether its deadline had passed by the store clock when it was read.
type CallOutcome struct {
	Key            CallOutcomeKey
	IdempotencyKey string
	Tool           ToolReference
	Origin         CallOrigin
	Status         CallOutcomeStatus
	DispatchFence  int64
	DispatchedAt   time.Time
	DeadlineAt     time.Time
	DeadlinePassed bool
	Result         ObjectRef
	Resolution     CallResolution
	ResolvedFence  int64
	Resolver       PrincipalRef
	ResolvedAt     time.Time
	Note           string
	Evidence       ObjectRef
}

// CallRecord is the stored result object of a settled or accepted call, what the call's
// observation is journaled from when the loop meets it unobserved.
type CallRecord struct {
	Output       []byte             `json:"output,omitempty"`
	IsError      bool               `json:"is_error,omitempty"`
	Usage        Usage              `json:"usage,omitzero"`
	ResourceURIs []string           `json:"resource_uris,omitempty"`
	Effect       *EffectObservation `json:"effect,omitempty"`
	Acceptance   *ToolAcceptance    `json:"acceptance,omitempty"`
}

// CallSettlement is a definite outcome of a call, completed or failed, with what settled it.
type CallSettlement struct {
	Key        CallOutcomeKey
	Status     CallOutcomeStatus
	Resolution CallResolution
	Record     CallRecord
}

// Validate refuses a settlement that is not definite or names no source.
func (settlement CallSettlement) Validate() error {
	if err := settlement.Key.Validate(); err != nil {
		return err
	}
	if !settlement.Status.Definite() || !settlement.Resolution.Valid() || settlement.Resolution == ResolvedByAttestation {
		return fmt.Errorf("%w: a call settles completed or failed by a gateway, worker, effect, lookup, or confirmation", ErrValidation)
	}
	return nil
}

// CallReport is a reconciliation source's answer about a dispatched call: CallCompleted with the
// tool's Result, CallFailed, or CallUnknown when the source cannot tell. Resolution is the source.
// OutputWithheld reports a completed call whose output may not be shown, such as one a guardrail blocked.
type CallReport struct {
	Status         CallOutcomeStatus
	Resolution     CallResolution
	Result         ToolResult
	Reason         string
	OutputWithheld bool
}

// LoopOutcome is the resolution an observation journaled from an outcome record carries.
type LoopOutcome struct {
	Status     CallOutcomeStatus `json:"status"`
	Resolution CallResolution    `json:"resolution"`
	Resolver   PrincipalRef      `json:"resolver,omitzero"`
	Evidence   ObjectRef         `json:"evidence,omitzero"`
}

// CallAttestation is one person's statement of an unknown call's outcome, completed or failed,
// with a mandatory note and optional evidence.
type CallAttestation struct {
	Key      CallOutcomeKey
	Attester PrincipalRef
	Outcome  CallOutcomeStatus
	Note     string
	Evidence ObjectRef
	// Persons is how many distinct authorized people must agree before the call settles.
	Persons int
	// TurnFence is the fence the person saw; the turn must still stand at it.
	TurnFence int64
}

// MaxAttestationNote bounds an attestation note, the width of its column.
const MaxAttestationNote = 500

// Validate refuses an attestation that is not definite, carries no note, or a malformed evidence reference.
func (attestation CallAttestation) Validate() error {
	if err := attestation.Key.Validate(); err != nil {
		return err
	}
	note := strings.TrimSpace(attestation.Note)
	switch {
	case !attestation.Outcome.Definite():
		return fmt.Errorf("%w: an attested outcome is completed or failed", ErrValidation)
	case note == "" || utf8.RuneCountInString(note) > MaxAttestationNote:
		return fmt.Errorf("%w: an attestation note is 1 to %d characters", ErrValidation, MaxAttestationNote)
	case attestation.Attester.Kind == "" || strings.TrimSpace(attestation.Attester.ID) == "":
		return fmt.Errorf("%w: an attestation needs its person", ErrPrincipalUnknown)
	case (attestation.Evidence.URI == "") != (attestation.Evidence.Digest == "") ||
		attestation.Evidence.Digest != "" && len(attestation.Evidence.Digest) != 64:
		return fmt.Errorf("%w: attestation evidence is a uri with a sha-256 digest", ErrValidation)
	case attestation.Persons < 1 || attestation.Persons > 2 || attestation.TurnFence <= 0:
		return fmt.Errorf("%w: an attestation needs one or two persons and the fence the person saw", ErrValidation)
	}
	return nil
}

// AttestationResult is what one attestation did: the call's outcome as it stands, and whether
// it still awaits a second person.
type AttestationResult struct {
	Outcome       CallOutcome
	AwaitsSecond  bool
	AlreadyStated bool
}

// TurnOutcomeRequest names one call of one turn of an agent at the turn fence the operator saw.
type TurnOutcomeRequest struct {
	AgentID         string
	RequestID       string
	Fence           int64
	ExecutionStepID int64
	CallID          string
}

// TurnAttestRequest attests the outcome of one call of a turn.
type TurnAttestRequest struct {
	TurnOutcomeRequest
	Outcome  CallOutcomeStatus
	Note     string
	Evidence ObjectRef
}

// ToolDispatchWindow bounds how long a sent call can stay in flight: a tool's timeout over all
// its attempts, each defaulting to Timeout and Attempts when the tool registers none, plus Grace
// before a person may attest the call's outcome.
type ToolDispatchWindow struct {
	Timeout  time.Duration
	Attempts int
	Grace    time.Duration
}

// Validate refuses a window that bounds nothing.
func (window ToolDispatchWindow) Validate() error {
	if window.Timeout <= 0 || window.Attempts <= 0 || window.Grace < 0 {
		return fmt.Errorf("%w: a dispatch window needs a positive tool timeout and attempts and a non-negative grace", ErrValidation)
	}
	return nil
}

// For is the dispatch window of one tool.
func (window ToolDispatchWindow) For(definition ToolDefinition) time.Duration {
	timeout, attempts := window.Timeout, window.Attempts
	if definition.Timeout > 0 {
		timeout = definition.Timeout
	}
	if definition.MaxAttempts > 0 {
		attempts = definition.MaxAttempts
	}
	return timeout*time.Duration(attempts) + window.Grace
}
