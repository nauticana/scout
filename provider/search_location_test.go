package provider

import (
	"testing"

	"github.com/nauticana/scout/domain"
)

// The probe must agree with each adapter's own refusal for every shape.
func TestSupportsSearchLocationMatchesTheAdapters(t *testing.T) {
	latitude, longitude := 30.2672, -97.7431
	shapes := map[string]domain.SearchLocation{
		"named":       {City: "Austin", Country: "US"},
		"coordinates": {Latitude: &latitude, Longitude: &longitude},
		"both":        {City: "Austin", Latitude: &latitude, Longitude: &longitude},
		"malformed":   {Country: "usa"},
	}
	selection := domain.ModelSelection{Model: "m"}
	adapters := map[string]func(domain.ModelRequest) error{
		GoogleProviderID: func(request domain.ModelRequest) error {
			_, _, err := (&Google{}).contentParams(request)
			return err
		},
		OpenAIProviderID: func(request domain.ModelRequest) error {
			_, err := (&OpenAI{}).completionParams(selection, request)
			return err
		},
		AnthropicProviderID: func(request domain.ModelRequest) error {
			_, err := (&Anthropic{}).messageParams(selection, request)
			return err
		},
	}
	for providerID, build := range adapters {
		for name, location := range shapes {
			accepted := build(locatedRequest(location)) == nil
			if got := SupportsSearchLocation(providerID, location); got != accepted {
				t.Errorf("%s %s: SupportsSearchLocation = %v, adapter accepts = %v", providerID, name, got, accepted)
			}
		}
	}
	if SupportsSearchLocation("unknown", shapes["named"]) {
		t.Fatal("an unknown provider reported support")
	}
}
