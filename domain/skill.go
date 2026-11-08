package domain

import (
	"fmt"
	"strings"
)

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
	// Requires are the skill versions the procedure relies on, tenant versions on a tenant
	// skill and catalog versions on a catalog skill; a release binding a tenant skill must
	// bind each at that version, and a copy of a catalog skill requires a copy of each.
	Requires []SkillReference
	// InputSchema, when set, is the JSON Schema of what the procedure needs before it starts.
	InputSchema []byte
	// Examples are requests that should select this skill; they seed its selection eval.
	Examples []string
	// EvalSet is the frozen golden set that gates releases binding this skill.
	EvalSet *GoldenSetReference
	// DerivedFrom is the platform catalog version a tenant version was copied from.
	DerivedFrom *SkillReference
	// Origin is the external source a catalog version was imported from.
	Origin *SkillOrigin
}

// SkillOriginKind is the kind of external source a catalog version was imported from.
type SkillOriginKind string

const (
	SkillOriginRepository SkillOriginKind = "R"
	SkillOriginDirectory  SkillOriginKind = "D"
	SkillOriginRegistry   SkillOriginKind = "G"
	SkillOriginFile       SkillOriginKind = "F"
)

// Valid reports whether the kind is one of the seeded codes.
func (kind SkillOriginKind) Valid() bool {
	switch kind {
	case SkillOriginRepository, SkillOriginDirectory, SkillOriginRegistry, SkillOriginFile:
		return true
	}
	return false
}

// SkillOrigin is where a catalog version's procedure was imported from, pinned by an
// immutable Revision, a "sha256:<64 hex>" Digest of the content, or both.
type SkillOrigin struct {
	Kind     SkillOriginKind
	Locator  string
	Revision string
	Digest   string
	// License is the identifier or text the source states; rights decisions stay with the importer.
	License string
}

// Normalized trims the origin and reports why it is not a valid pin.
func (origin SkillOrigin) Normalized() (SkillOrigin, error) {
	origin.Locator, origin.Revision = strings.TrimSpace(origin.Locator), strings.TrimSpace(origin.Revision)
	origin.Digest, origin.License = strings.TrimSpace(origin.Digest), strings.TrimSpace(origin.License)
	switch {
	case !origin.Kind.Valid():
		return origin, fmt.Errorf("%w: skill origin kind %q is not a seeded code", ErrValidation, origin.Kind)
	case origin.Locator == "":
		return origin, fmt.Errorf("%w: skill origin needs a locator", ErrValidation)
	case origin.Revision == "" && origin.Digest == "":
		return origin, fmt.Errorf("%w: skill origin needs an immutable revision or a content digest", ErrValidation)
	case origin.Digest != "" && !validSHA256Digest(origin.Digest):
		return origin, fmt.Errorf("%w: skill origin digest must be sha256:<64 hex>", ErrValidation)
	}
	return origin, nil
}

func validSHA256Digest(digest string) bool {
	hexDigest, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || len(hexDigest) != 64 {
		return false
	}
	for _, r := range hexDigest {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
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
