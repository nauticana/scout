package domain

// UseSkillToolID is the tool an agent calls to load one bound skill's procedure.
const UseSkillToolID = "use_skill"

// SkillDefinition is an immutable tenant procedure. An agent sees only its Summary
// in the skill index and loads the rest through use_skill.
type SkillDefinition struct {
	SkillID     string
	Version     string
	DisplayName string
	Summary     string
	// Procedure is the instructions, typically markdown.
	Procedure string
	// Tools are the tool ids the procedure uses; a release binding the skill must bind each.
	Tools []string
	// InputSchema, when set, is the JSON Schema of what the procedure needs before it starts.
	InputSchema []byte
	// Examples are requests that should select this skill; they seed its selection eval.
	Examples []string
	// EvalSet is the frozen golden set that gates releases binding this skill.
	EvalSet *GoldenSetReference
}

// SkillReference names one registered skill version.
type SkillReference struct {
	SkillID string `json:"skill_id"`
	Version string `json:"version"`
}

// GoldenSetReference names one frozen golden set version.
type GoldenSetReference struct {
	GoldenSetID string
	SetVersion  int64
}
