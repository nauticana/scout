package provider

import (
	"fmt"

	"github.com/nauticana/scout/domain"
)

// SupportsSearchLocation reports whether the adapter for providerID can run a
// grounded search from location, by the same check its Generate applies. An
// invalid location or an unknown provider reports false.
func SupportsSearchLocation(providerID string, location domain.SearchLocation) bool {
	if (&domain.SearchGrounding{Location: &location}).Validate() != nil {
		return false
	}
	return searchLocationSupport(providerID, location) == nil
}

// searchLocationSupport refuses a location the provider's vendor cannot honour:
// Google locates searches by coordinates only, OpenAI and Anthropic by place name.
func searchLocationSupport(providerID string, location domain.SearchLocation) error {
	switch providerID {
	case GoogleProviderID:
		if location.Latitude == nil {
			return fmt.Errorf("%w: %s adapter locates searches by coordinates only", domain.ErrCapabilityUnsupported, providerID)
		}
	case OpenAIProviderID, AnthropicProviderID:
		if location.City == "" && location.Region == "" && location.Country == "" && location.Timezone == "" {
			return fmt.Errorf("%w: %s adapter locates searches by city, region, country or timezone, not coordinates", domain.ErrCapabilityUnsupported, providerID)
		}
	default:
		return fmt.Errorf("%w: provider %q has no search location support", domain.ErrCapabilityUnsupported, providerID)
	}
	return nil
}
