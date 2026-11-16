package evaluation

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// withheldSkillSandbox answers use_skill for the skill a replay withholds as the runtime answers
// a skill the release does not bind; the call is still recorded.
type withheldSkillSandbox struct {
	contract.ToolSandbox
	skillID string
}

func (sandbox withheldSkillSandbox) Invoke(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
	result, err := sandbox.ToolSandbox.Invoke(ctx, call)
	if call.ToolID != domain.UseSkillToolID {
		return result, err
	}
	var args struct {
		Skill string `json:"skill"`
	}
	if json.Unmarshal(call.Arguments, &args) == nil && args.Skill == sandbox.skillID {
		return domain.ToolResult{}, fmt.Errorf("%w: the replay withholds skill %q", domain.ErrNotFound, sandbox.skillID)
	}
	return result, err
}
