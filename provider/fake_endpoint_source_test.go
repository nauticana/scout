package provider

import (
	"context"
	"fmt"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

type fakeEndpointSource map[string]domain.ModelEndpoint

var _ contract.ModelEndpointSource = fakeEndpointSource(nil)

func (source fakeEndpointSource) ModelEndpoint(_ context.Context, providerID string) (domain.ModelEndpoint, error) {
	endpoint, found := source[providerID]
	if !found {
		return domain.ModelEndpoint{}, fmt.Errorf("%w: %s", domain.ErrNotFound, providerID)
	}
	return endpoint, nil
}
