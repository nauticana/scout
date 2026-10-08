package confirmation

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/nauticana/scout/domain"
)

const (
	statePrefix   = "mcp-confirmation:"
	elicitationID = "confirmation"
	approveField  = "approve"
)

var confirmSchema = json.RawMessage(`{"type":"object","properties":{"approve":{"type":"boolean","title":"Approve this action",` +
	`"description":"Yes runs exactly the action shown. No declines it and nothing happens."}},"required":["approve"]}`)

// ToolResult reports a confirmation to the MCP client. A pending elicitation
// confirmation shown to its own maker and client asks for the decision with a
// form whose message is the full preview text; anything else returns its
// status, the inbox link while it waits, and the stored result once executed.
func ToolResult(confirmation domain.MCPConfirmation, viewer domain.PrincipalRef, clientRef, previewText, approvalURL string) domain.MCPToolResult {
	waiting := confirmation.Status == domain.MCPConfirmationPending && !confirmation.Expired
	status := confirmation.Status
	if confirmation.Status == domain.MCPConfirmationPending && confirmation.Expired {
		status = domain.MCPConfirmationExpired
	}
	data := map[string]any{
		"confirmation_id": confirmation.ID,
		"status":          string(status),
		"tool":            confirmation.Tool,
		"expires_at":      confirmation.ExpiresAt.UTC().Format(time.RFC3339),
	}
	if confirmation.ApprovalID > 0 {
		data["approval_id"] = confirmation.ApprovalID
	}
	if confirmation.Error != "" {
		data["reason"] = confirmation.Error
	}
	if len(confirmation.Preview) > 0 {
		data["preview"] = confirmation.Preview
	}
	if len(confirmation.Result) > 0 && confirmation.Status == domain.MCPConfirmationExecuted {
		data["result"] = confirmation.Result
	}
	if waiting && approvalURL != "" {
		data["approval_url"] = approvalURL
	}
	result := domain.MCPToolResult{Data: data}
	if waiting && confirmation.Channel == domain.MCPConfirmationElicitation && strings.TrimSpace(previewText) != "" &&
		confirmation.Maker == viewer && confirmation.ClientRef == clientRef {
		result.Elicit = map[string]domain.MCPElicitation{elicitationID: {Message: previewText, Schema: confirmSchema}}
		result.State = statePrefix + strconv.FormatInt(confirmation.ID, 10)
	}
	return result
}

// Answered reads the decision a repeated call carries: the confirmation its
// untrusted state names and whether the person approved. ok is false when the
// call answers no confirmation. Anything but an accepted yes is a decline.
func Answered(call domain.MCPToolCall) (id int64, approve bool, ok bool) {
	rest, found := strings.CutPrefix(call.State, statePrefix)
	if !found {
		return 0, false, false
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || id <= 0 {
		return 0, false, false
	}
	answer, answered := call.Elicited[elicitationID]
	if !answered {
		return 0, false, false
	}
	if answer.Action == domain.MCPElicitationAccept {
		approve, _ = answer.Content[approveField].(bool)
	}
	return id, approve, true
}
