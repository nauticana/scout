package modelgateway

import (
	"context"
	"reflect"
	"testing"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/internal/fake"
)

func TestMediaGatewayAppliesControlsAndReleasesCapacity(t *testing.T) {
	var calls []string
	provider := mediaProviderFunc(func(context.Context, string, domain.ImageRequest) ([]domain.GeneratedMedia, error) {
		calls = append(calls, "generate")
		return []domain.GeneratedMedia{{MimeType: "image/png"}}, nil
	})
	gateway, err := NewMediaGateway(
		&fake.TenantRateLimiter{AllowModelCallFunc: func(context.Context, domain.ModelRequest) error {
			calls = append(calls, "rate")
			return nil
		}},
		provider,
		fake.CapacitySchedulerFunc(func(context.Context, domain.ModelRequest, domain.ModelSelection) (contract.CapacityLease, error) {
			calls = append(calls, "capacity")
			return &fake.CapacityLease{PoolValue: "shared", ReleaseFunc: func(context.Context, domain.Usage) error {
				calls = append(calls, "release")
				return nil
			}}, nil
		}),
		"provider",
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = gateway.GenerateImage(context.Background(), "image-model", domain.ImageRequest{
		TenantContext: domain.TenantContext{TenantID: 7}, RequestID: "request", Prompt: "draw",
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"rate", "capacity", "generate", "release"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

type mediaProviderFunc func(context.Context, string, domain.ImageRequest) ([]domain.GeneratedMedia, error)

func (provider mediaProviderFunc) GenerateImage(ctx context.Context, model string, request domain.ImageRequest) ([]domain.GeneratedMedia, error) {
	return provider(ctx, model, request)
}

func (mediaProviderFunc) GenerateVideo(context.Context, string, domain.VideoRequest) ([]domain.GeneratedMedia, error) {
	return nil, nil
}

var _ contract.MediaProvider = mediaProviderFunc(nil)
