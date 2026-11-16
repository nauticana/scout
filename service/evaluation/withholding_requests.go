package evaluation

import (
	"context"
	"fmt"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/service/dataplane"
)

// withholdingRequests builds a replay's requests under its skill withholding and refuses, before
// the model is called, a builder that left the skill in the index.
type withholdingRequests struct {
	contract.ToolLoopRequestBuilder
	withholding *dataplane.SkillWithholding
}

func (builder withholdingRequests) Build(ctx context.Context, input domain.StepInput, config domain.ToolLoopConfig) (domain.ModelRequest, domain.Usage, error) {
	request, spent, err := builder.ToolLoopRequestBuilder.Build(builder.withholding.Context(ctx), input, config)
	if err == nil && !builder.withholding.Honoured() {
		return domain.ModelRequest{}, spent, fmt.Errorf("%w: the request builder does not withhold skill %q", domain.ErrCapabilityUnsupported, builder.withholding.SkillID)
	}
	return request, spent, err
}
