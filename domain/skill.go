package domain

// UseSkillToolID is the tool an agent calls to load one bound skill's procedure.
const UseSkillToolID = "use_skill"

// SkillDefinition is an immutable procedure, owned by a tenant or by the platform
// skill catalog. An agent sees only its Summary in the skill index and loads the
// rest through use_skill.
type SkillDefinition struct {
	SkillID     string
	Version     string
	DisplayName string
	Summary     string
	// Procedure is the instructions, typically markdown.
	Procedure string
	// Tools are the tool ids the procedure uses; a release binding the skill must bind each.
	Tools []string
	// Requires are the tenant skill versions the procedure relies on; a release
	// binding the skill must bind each at that version.
	Requires []SkillReference
	// InputSchema, when set, is the JSON Schema of what the procedure needs before it starts.
	InputSchema []byte
	// Examples are requests that should select this skill; they seed its selection eval.
	Examples []string
	// EvalSet is the frozen golden set that gates releases binding this skill.
	EvalSet *GoldenSetReference
	// DerivedFrom is the platform catalog version a tenant version was copied from.
	DerivedFrom *SkillReference
}

// SkillReference names one registered skill version.
type SkillReference struct {
	SkillID string `json:"skill_id"`
	Version string `json:"version"`
}

// TenantSkillReference names one tenant's skill version.
type TenantSkillReference struct {
	TenantID int64
	SkillID  string
	Version  string
}

// GoldenSetReference names one frozen golden set version.
type GoldenSetReference struct {
	GoldenSetID string
	SetVersion  int64
}
