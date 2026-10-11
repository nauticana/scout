package scope

import (
	"bytes"
	"encoding/json"
	"sort"

	"github.com/nauticana/scout/domain"
)

// PromptSectionChanges lists the compiled prompt sections after adds, changes, or removes
// against before, ordered by language and section.
func PromptSectionChanges(before, after []domain.CompiledPrompt) []domain.PromptSectionChange {
	type key struct {
		language string
		section  int64
	}
	index := func(languages []domain.CompiledPrompt) map[key]domain.CompiledPromptSection {
		sections := map[key]domain.CompiledPromptSection{}
		for _, language := range languages {
			for _, section := range language.Sections {
				sections[key{language.LanguageCode, section.PromptSectionID}] = section
			}
		}
		return sections
	}
	old, current := index(before), index(after)
	var changes []domain.PromptSectionChange
	for k, section := range current {
		previous, existed := old[k]
		switch {
		case !existed:
			changes = append(changes, domain.PromptSectionChange{LanguageCode: k.language, PromptSectionID: k.section, Caption: section.Caption, Change: domain.DiffAdded})
		case previous.Sequence != section.Sequence || previous.Caption != section.Caption || previous.Description != section.Description ||
			previous.Instruction != section.Instruction || previous.Output != section.Output:
			changes = append(changes, domain.PromptSectionChange{LanguageCode: k.language, PromptSectionID: k.section, Caption: section.Caption, Change: domain.DiffChanged})
		}
	}
	for k, section := range old {
		if _, kept := current[k]; !kept {
			changes = append(changes, domain.PromptSectionChange{LanguageCode: k.language, PromptSectionID: k.section, Caption: section.Caption, Change: domain.DiffRemoved})
		}
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].LanguageCode != changes[j].LanguageCode {
			return changes[i].LanguageCode < changes[j].LanguageCode
		}
		return changes[i].PromptSectionID < changes[j].PromptSectionID
	})
	return changes
}

// SettingChanges lists the effective settings other than prompt sections that after adds,
// changes, or removes against before, ordered by kind and id.
func SettingChanges(before, after []domain.EffectiveResource) []domain.SettingChange {
	key := func(resource domain.EffectiveResource) string {
		return string(resource.ResourceKind) + "\x00" + resource.ResourceID
	}
	old := map[string]domain.EffectiveResource{}
	for _, resource := range before {
		if resource.ResourceKind != domain.ResourcePromptSection {
			old[key(resource)] = resource
		}
	}
	var changes []domain.SettingChange
	for _, resource := range after {
		if resource.ResourceKind == domain.ResourcePromptSection {
			continue
		}
		previous, existed := old[key(resource)]
		delete(old, key(resource))
		switch {
		case !existed:
			changes = append(changes, domain.SettingChange{ResourceKind: resource.ResourceKind, ResourceID: resource.ResourceID,
				Change: domain.DiffAdded, After: json.RawMessage(resource.Value)})
		case !bytes.Equal(previous.Value, resource.Value):
			changes = append(changes, domain.SettingChange{ResourceKind: resource.ResourceKind, ResourceID: resource.ResourceID,
				Change: domain.DiffChanged, Before: json.RawMessage(previous.Value), After: json.RawMessage(resource.Value)})
		}
	}
	for _, resource := range old {
		changes = append(changes, domain.SettingChange{ResourceKind: resource.ResourceKind, ResourceID: resource.ResourceID,
			Change: domain.DiffRemoved, Before: json.RawMessage(resource.Value)})
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].ResourceKind != changes[j].ResourceKind {
			return changes[i].ResourceKind < changes[j].ResourceKind
		}
		return changes[i].ResourceID < changes[j].ResourceID
	})
	return changes
}
