package dataplane

import (
	"context"
	"slices"
	"sync/atomic"

	"github.com/nauticana/scout/domain"
)

type skillWithholdingKey struct{}

// SkillWithholding is one replay's instruction to leave a bound skill out of the skill index.
// A request builder honours it by passing its skills through SkillWithholdingFrom(ctx).Drop, as
// ReleaseLoopRequestBuilder does; Honoured lets the replay refuse a builder that did not.
type SkillWithholding struct {
	SkillID  string
	honoured atomic.Bool
}

// Context scopes ctx to the withholding.
func (withholding *SkillWithholding) Context(ctx context.Context) context.Context {
	return context.WithValue(ctx, skillWithholdingKey{}, withholding)
}

// Honoured reports whether a request builder dropped the skill from an index.
func (withholding *SkillWithholding) Honoured() bool { return withholding.honoured.Load() }

// SkillWithholdingFrom returns the withholding ctx is scoped to; nil when there is none.
func SkillWithholdingFrom(ctx context.Context) *SkillWithholding {
	withholding, _ := ctx.Value(skillWithholdingKey{}).(*SkillWithholding)
	return withholding
}

// Drop returns skills without the withheld one and marks the withholding honoured; a nil
// withholding drops nothing.
func (withholding *SkillWithholding) Drop(skills []domain.SkillDefinition) []domain.SkillDefinition {
	if withholding == nil {
		return skills
	}
	withholding.honoured.Store(true)
	return slices.DeleteFunc(skills, func(skill domain.SkillDefinition) bool { return skill.SkillID == withholding.SkillID })
}
