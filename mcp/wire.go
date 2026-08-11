package mcp

import (
	"github.com/nauticana/scout/api"
	"github.com/nauticana/scout/domain"
)

// Projections from transport-neutral values onto the mcp-v1 wire profile.
// Identical field sets convert directly; the build breaks loudly if either
// side drifts.

// WireField projects one discovered field onto its wire form.
func WireField(field domain.FieldDescriptor) api.FieldDescriptor {
	return api.FieldDescriptor(field)
}

// WireFields projects a field catalog result onto its wire form.
func WireFields(fields []domain.FieldDescriptor) []api.FieldDescriptor {
	wire := make([]api.FieldDescriptor, len(fields))
	for i, field := range fields {
		wire[i] = WireField(field)
	}
	return wire
}

func wireMeta(meta *domain.EnvelopeMeta) *api.EnvelopeMeta {
	if meta == nil {
		return nil
	}
	return &api.EnvelopeMeta{
		GeneratedAt: meta.GeneratedAt,
		Source:      meta.Source,
		Provenance:  wireProvenance(meta.Provenance),
		Pagination:  wirePagination(meta.Pagination),
	}
}

func wireProvenance(provenance *domain.ProvenanceMeta) *api.ProvenanceMeta {
	if provenance == nil {
		return nil
	}
	sources := make([]api.SourceAttrib, len(provenance.Sources))
	for i, source := range provenance.Sources {
		sources[i] = api.SourceAttrib(source)
	}
	return &api.ProvenanceMeta{
		VerificationLevel: provenance.VerificationLevel,
		CompletenessScore: provenance.CompletenessScore,
		UpdatedAt:         provenance.UpdatedAt,
		VerifiedAt:        provenance.VerifiedAt,
		Sources:           sources,
		Attribution:       provenance.Attribution,
	}
}

func wirePagination(pagination *domain.PaginationMeta) *api.PaginationMeta {
	if pagination == nil {
		return nil
	}
	return (*api.PaginationMeta)(pagination)
}
