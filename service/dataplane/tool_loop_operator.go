package dataplane

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/guardrail"
)

// withdrawnOutput is what the model sees of a call a person withdrew before it was sent.
var withdrawnOutput = []byte(`{"error":"withdrawn"}`)

// drainOperator adds, before the next decision, one operator-labelled message holding every entry
// people made at a position the journal cursor reached. Entries are immutable and their positions
// fixed, so a replay adds the same message at the same place; the journal itself is unchanged.
func (run *loopRun) drainOperator() {
	var message strings.Builder
	for ; run.drained < len(run.input.Operator); run.drained++ {
		entry := run.input.Operator[run.drained]
		if entry.Position.AfterEntryNo > run.cursor {
			break
		}
		if label, text := operatorText(entry); text != "" {
			message.WriteString(guardrail.MarkUntrusted(label, text))
		}
	}
	if message.Len() == 0 {
		return
	}
	text := guardrail.UntrustedContentNotice + "A person operated this turn and left these entries:\n" + message.String()
	run.request.Messages = append(run.request.Messages, domain.ModelMessage{Role: domain.ModelRoleUser, Text: []byte(text)})
}

// operatorText is one entry as the model reads it; a withdrawal is the withdrawn call's
// observation instead.
func operatorText(entry domain.OperatorEntry) (string, string) {
	body := entry.Body
	switch entry.Kind {
	case domain.OperatorNote:
		return "Operator note", body.Text
	case domain.OperatorHandback:
		return "Operator handback instruction", body.Text
	case domain.OperatorReply:
		if body.Frame != nil {
			return "Operator reply sent to the user", string(body.Frame.Payload)
		}
	case domain.OperatorTool:
		if use := body.Tool; use != nil {
			return fmt.Sprintf("Operator call of tool %s@%s (%s)", use.ToolID, use.Version, operatorCallStatus(use.Status)),
				fmt.Sprintf("arguments: %s\noutput: %s", use.Arguments, use.Output)
		}
	}
	return "", ""
}

func operatorCallStatus(status domain.CallOutcomeStatus) string {
	switch status {
	case domain.CallCompleted:
		return "completed"
	case domain.CallFailed:
		return "failed"
	}
	return "outcome unknown"
}

// withdrawal finds a person's withdrawal of the call at the cursor among the entries the cursor reached.
func (run *loopRun) withdrawal(callID string) (domain.OperatorEntry, bool) {
	for _, entry := range run.input.Operator {
		if entry.Kind == domain.OperatorWithdraw && entry.Body.CallID == callID && entry.Position.AfterEntryNo <= run.cursor {
			return entry, true
		}
	}
	return domain.OperatorEntry{}, false
}

// withdraw journals a withdrawn call's observation, failed and attested by the person who withdrew
// it; the call is never sent.
func (executor *ToolLoopExecutor) withdraw(ctx context.Context, run *loopRun, iteration int, call domain.ModelToolCall, reference domain.ToolReference, withdrawal domain.OperatorEntry, wasPending bool) (domain.ModelToolObservation, error) {
	output := json.RawMessage(withdrawnOutput)
	entry, err := executor.append(ctx, run, domain.LoopEntry{
		Kind: domain.LoopEntryObservation, Iteration: iteration, Tool: reference,
		Observation: &domain.ModelToolObservation{CallID: call.CallID, Name: call.Name, Output: output, IsError: true},
		Outcome:     &domain.LoopOutcome{Status: domain.CallFailed, Resolution: domain.ResolvedByAttestation, Resolver: withdrawal.Actor},
	})
	if err != nil {
		return domain.ModelToolObservation{}, err
	}
	run.recordCall(call, reference, entry, wasPending)
	return *entry.Observation, run.account(entry.Usage)
}
