package modelgateway

import (
	"context"
	"reflect"
	"testing"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

func TestMediaGatewayAppliesControlsAndReleasesCapacity(t *testing.T) {
	var calls []string
	var released domain.Usage
	provider := mediaProviderFunc(func(context.Context, string, domain.ImageRequest) (domain.MediaResult, error) {
		calls = append(calls, "generate")
		return domain.MediaResult{Media: []domain.GeneratedMedia{{MimeType: "image/png"}}, Usage: domain.ModelUsage{Images: 1, InputTokens: 20, OutputTokens: 4000}}, nil
	})
	gateway, err := NewMediaGateway(
		&fake.TenantRateLimiter{AllowModelCallFunc: func(context.Context, domain.ModelRequest) error {
			calls = append(calls, "rate")
			return nil
		}},
		provider,
		fake.CapacitySchedulerFunc(func(context.Context, domain.ModelRequest, domain.ModelSelection) (contract.CapacityLease, error) {
			calls = append(calls, "capacity")
			return &fake.CapacityLease{PoolValue: "shared", ReleaseFunc: func(_ context.Context, usage domain.Usage) error {
				calls = append(calls, "release")
				released = usage
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
	if released.InputTokens != 20 || released.OutputTokens != 4000 {
		t.Fatalf("released usage = %+v, want the image tokens", released)
	}
}

type mediaProviderFunc func(context.Context, string, domain.ImageRequest) (domain.MediaResult, error)

func (provider mediaProviderFunc) GenerateImage(ctx context.Context, model string, request domain.ImageRequest) (domain.MediaResult, error) {
	return provider(ctx, model, request)
}

func (mediaProviderFunc) GenerateVideo(context.Context, string, domain.VideoRequest) (domain.MediaResult, error) {
	return domain.MediaResult{}, nil
}

var _ contract.MediaProvider = mediaProviderFunc(nil)
