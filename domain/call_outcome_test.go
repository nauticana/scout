package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestToolAcceptanceIsValidatedBeforeALookupCanUseIt(t *testing.T) {
	if err := (ToolAcceptance{Handle: "job-7", LookupWithin: time.Hour, MCPConfirmationID: 41}).Validate(); err != nil {
		t.Fatal(err)
	}
	for name, acceptance := range map[string]ToolAcceptance{
		"blank handle":    {Handle: " ", LookupWithin: time.Hour},
		"long handle":     {Handle: strings.Repeat("h", 513), LookupWithin: time.Hour},
		"control byte":    {Handle: "job\n7", LookupWithin: time.Hour},
		"no deadline":     {Handle: "job-7"},
		"beyond the wait": {Handle: "job-7", LookupWithin: MaxAcceptanceWait + time.Second},
		"negative link":   {Handle: "job-7", LookupWithin: time.Hour, MCPConfirmationID: -1},
	} {
		if err := acceptance.Validate(); !errors.Is(err, ErrInvalidModelOutput) {
			t.Errorf("%s = %v, want invalid output", name, err)
		}
	}
}

func TestCallOutcomeCodesClassifyOpenAndDefinite(t *testing.T) {
	for _, status := range []CallOutcomeStatus{CallInFlight, CallAccepted, CallUnknown} {
		if !status.Open() || status.Definite() {
			t.Errorf("%s must be open", status)
		}
	}
	for _, status := range []CallOutcomeStatus{CallCompleted, CallFailed} {
		if status.Open() || !status.Definite() {
			t.Errorf("%s must be definite", status)
		}
	}
	key := CallOutcomeKey{TenantID: 7, RequestID: "r", ExecutionStepID: 1, CallID: "c"}
	if err := (CallSettlement{Key: key, Status: CallUnknown, Resolution: ResolvedByGateway}).Validate(); !errors.Is(err, ErrValidation) {
		t.Fatalf("an open settlement = %v", err)
	}
	if err := (CallSettlement{Key: key, Status: CallCompleted, Resolution: ResolvedByAttestation}).Validate(); !errors.Is(err, ErrValidation) {
		t.Fatalf("an attestation is not a settlement: %v", err)
	}
	if err := (CallOutcomeKey{TenantID: 7, RequestID: "r", ExecutionStepID: 1, CallID: strings.Repeat("c", 201)}).Validate(); !errors.Is(err, ErrValidation) {
		t.Fatalf("a call id wider than its column = %v", err)
	}
}

func TestDispatchWindowSpansEveryAttemptPlusTheGrace(t *testing.T) {
	window := ToolDispatchWindow{Timeout: 30 * time.Second, Attempts: 3, Grace: time.Minute}
	if got := window.For(ToolDefinition{}); got != 150*time.Second {
		t.Fatalf("default window = %s", got)
	}
	if got := window.For(ToolDefinition{Timeout: 10 * time.Second, MaxAttempts: 1}); got != 70*time.Second {
		t.Fatalf("registered window = %s", got)
	}
	if err := (ToolDispatchWindow{Timeout: time.Second}).Validate(); !errors.Is(err, ErrValidation) {
		t.Fatalf("a window without attempts = %v", err)
	}
}

func TestCallAttestationNeedsAPersonANoteAndOneOrTwoPersons(t *testing.T) {
	valid := CallAttestation{Key: CallOutcomeKey{TenantID: 7, RequestID: "r", ExecutionStepID: 1, CallID: "c"},
		Attester: PrincipalRef{Kind: PrincipalHuman, ID: "9"}, Outcome: CallCompleted, Note: "seen", Persons: 1, TurnFence: 3}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*CallAttestation){
		"open outcome":   func(a *CallAttestation) { a.Outcome = CallUnknown },
		"no note":        func(a *CallAttestation) { a.Note = "  " },
		"long note":      func(a *CallAttestation) { a.Note = strings.Repeat("n", MaxAttestationNote+1) },
		"half evidence":  func(a *CallAttestation) { a.Evidence = ObjectRef{URI: "scout://e"} },
		"three persons":  func(a *CallAttestation) { a.Persons = 3 },
		"no fence":       func(a *CallAttestation) { a.TurnFence = 0 },
		"no person":      func(a *CallAttestation) { a.Attester = PrincipalRef{} },
		"short evidence": func(a *CallAttestation) { a.Evidence = ObjectRef{URI: "scout://e", Digest: "d"} },
	} {
		attestation := valid
		mutate(&attestation)
		if err := attestation.Validate(); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
}
