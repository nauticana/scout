package runtime

import "github.com/nauticana/scout/domain"

// SQL literals of the closed codes the runtime statements name.
const (
	sqlRunCompleted = "'" + string(domain.RunCompleted) + "'"
	sqlAgentActive  = "'" + string(domain.AgentStateActive) + "'"
)
