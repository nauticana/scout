package skill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/toolgateway"
)

// UseSkillToolVersion is the version of the open use_skill contract, which accepts any skill id.
const UseSkillToolVersion = "1"

// UseSkillTool is the in-process tool contract a tenant registers and a release binds
// alongside its skills. With skill ids the input schema enumerates them and the version
// is derived from the set, so an agent type with a fixed skill set binds a contract the
// model cannot call with an unknown skill; publication then refuses a release that binds
// a skill the contract does not list.
func UseSkillTool(skillIDs ...string) domain.ToolDefinition {
	tool := domain.ToolDefinition{
		ToolID: domain.UseSkillToolID, Version: UseSkillToolVersion, DisplayName: "Use skill",
		Endpoint:    toolgateway.InProcessEndpoint(domain.UseSkillToolID),
		InputSchema: []byte(`{"type":"object","required":["skill"],"additionalProperties":false,"properties":{"skill":{"type":"string"}}}`),
		OutputSchema: []byte(`{"type":"object","required":["skill","procedure","tools"],"properties":{` +
			`"skill":{"type":"string"},"procedure":{"type":"string"},"tools":{"type":"array","items":{"type":"string"}},"input_schema":{"type":"object"}}}`),
	}
	if len(skillIDs) == 0 {
		return tool
	}
	ids := slices.Compact(slices.Sorted(slices.Values(skillIDs)))
	enum, _ := json.Marshal(ids)
	sum := sha256.Sum256(enum)
	tool.Version = UseSkillToolVersion + "-" + hex.EncodeToString(sum[:8])
	tool.InputSchema = []byte(`{"type":"object","required":["skill"],"additionalProperties":false,"properties":{"skill":{"type":"string","enum":` + string(enum) + `}}}`)
	return tool
}

// enumeratedSkills returns the skill ids a use_skill input schema enumerates; nil when it is open.
func enumeratedSkills(inputSchema []byte) ([]string, error) {
	var schema struct {
		Properties struct {
			Skill struct {
				Enum []string `json:"enum"`
			} `json:"skill"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(inputSchema, &schema); err != nil {
		return nil, fmt.Errorf("%w: %s input schema: %w", domain.ErrValidation, domain.UseSkillToolID, err)
	}
	return schema.Properties.Skill.Enum, nil
}

// UseSkill serves use_skill: the procedure of one skill the caller's pinned release binds.
type UseSkill struct {
	Skills contract.SkillRegistry
}

// Register serves use_skill from the transport.
func (tool *UseSkill) Register(transport *toolgateway.InProcessTransport) error {
	if tool.Skills == nil {
		return fmt.Errorf("%w: use_skill needs a skill registry", domain.ErrValidation)
	}
	return transport.Register(domain.UseSkillToolID, tool.Handle)
}

// Handle implements toolgateway.InProcessHandler.
func (tool *UseSkill) Handle(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
	var args struct {
		Skill string `json:"skill"`
	}
	if err := json.Unmarshal(call.Arguments, &args); err != nil || strings.TrimSpace(args.Skill) == "" {
		return domain.ToolResult{}, fmt.Errorf("%w: use_skill needs a skill id", domain.ErrValidation)
	}
	principal := call.Principal
	bound, err := tool.Skills.List(ctx, principal.TenantID, principal.ID, principal.Release)
	if err != nil {
		return domain.ToolResult{}, err
	}
	for _, skill := range bound {
		if skill.SkillID == args.Skill {
			return procedureResult(skill)
		}
	}
	return domain.ToolResult{}, fmt.Errorf("%w: %s@%s binds no skill %q", domain.ErrNotFound, principal.ID, principal.Release, args.Skill)
}

func procedureResult(skill domain.SkillDefinition) (domain.ToolResult, error) {
	output, err := json.Marshal(struct {
		Skill       string          `json:"skill"`
		Procedure   string          `json:"procedure"`
		Tools       []string        `json:"tools"`
		InputSchema json.RawMessage `json:"input_schema,omitempty"`
	}{skill.SkillID, skill.Procedure, append([]string{}, skill.Tools...), skill.InputSchema})
	if err != nil {
		return domain.ToolResult{}, fmt.Errorf("encode skill %s: %w", skill.SkillID, err)
	}
	return domain.ToolResult{Output: output}, nil
}

// Index is the skill list an agent chooses from before loading one procedure.
func Index(skills []domain.SkillDefinition) string {
	if len(skills) == 0 {
		return ""
	}
	var index strings.Builder
	index.WriteString("Skills (load the matching one with " + domain.UseSkillToolID + " before starting):\n")
	for _, skill := range skills {
		fmt.Fprintf(&index, "- %s: %s\n", skill.SkillID, skill.Summary)
	}
	return index.String()
}
