package controlplane

import "github.com/nauticana/scout/domain"

// SQL literals of the agent_state and priority_class codes the control plane's statements name.
const (
	sqlAgentDraft       = "'" + string(domain.AgentStateDraft) + "'"
	sqlAgentRetired     = "'" + string(domain.AgentStateRetired) + "'"
	sqlPriorityStandard = "'" + string(domain.PriorityStandard) + "'"
)
