package provider

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/genai"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

// Google invokes Gemini for text and Imagen/Veo for media, through either
// Vertex AI (ADC, needs Project+Location) or the Gemini Developer API (APIKey)
// on hosts without keyless Vertex access.
type Google struct {
	ProjectID    string
	Location     string
	UseGeminiAPI bool
	APIKey       string
	Temperature  float64
	// VideoPollInterval paces the long-running video operation; zero uses one second.
	VideoPollInterval time.Duration
}

var (
	_ contract.ModelProvider = (*Google)(nil)
	_ contract.MediaProvider = (*Google)(nil)
)

func (p *Google) newClient(ctx context.Context) (*genai.Client, error) {
	if p.UseGeminiAPI {
		return genai.NewClient(ctx, &genai.ClientConfig{Backend: genai.BackendGeminiAPI, APIKey: p.APIKey})
	}
	return genai.NewClient(ctx, &genai.ClientConfig{Project: p.ProjectID, Location: p.Location, Backend: genai.BackendVertexAI})
}

func (p *Google) Generate(ctx context.Context, selection domain.ModelSelection, request domain.ModelRequest) (domain.ModelResult, error) {
	client, err := p.newClient(ctx)
	if err != nil {
		return domain.ModelResult{}, fmt.Errorf("genai.NewClient: %w", err)
	}
	prompt := string(request.Prompt)
	resp, err := client.Models.GenerateContent(ctx, selection.Model, genai.Text(prompt), &genai.GenerateContentConfig{
		Temperature:     genai.Ptr(float32(temperature(p.Temperature))),
		MaxOutputTokens: int32(maxOutputTokens(request)),
	})
	if err != nil {
		return domain.ModelResult{}, fmt.Errorf("genai GenerateContent: %w", err)
	}
	text := resp.Text()
	if text == "" {
		return domain.ModelResult{}, fmt.Errorf("genai: no text content generated")
	}
	usage := domain.Usage{}
	if resp.UsageMetadata != nil {
		usage.InputTokens = int64(resp.UsageMetadata.PromptTokenCount)
		usage.OutputTokens = int64(resp.UsageMetadata.CandidatesTokenCount)
	} else {
		// Vertex omits usage on some model families; a 4-chars-per-token
		// estimate keeps accounting non-zero rather than silently free.
		usage.InputTokens = int64(len(prompt)) / 4
		usage.OutputTokens = int64(len(text)) / 4
	}
	return domain.ModelResult{Output: []byte(text), Usage: usage}, nil
}

func (p *Google) Stream(ctx context.Context, selection domain.ModelSelection, request domain.ModelRequest) (contract.ModelStream, error) {
	return singleFrameStream(ctx, p, selection, request)
}

func (p *Google) GenerateImage(ctx context.Context, model string, request domain.ImageRequest) ([]domain.GeneratedMedia, error) {
	if model == "" {
		return nil, fmt.Errorf("%w: no image model configured", domain.ErrValidation)
	}
	client, err := p.newClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("genai.NewClient: %w", err)
	}
	aspectRatio := request.AspectRatio
	if aspectRatio == "" {
		aspectRatio = "16:9"
	}
	resp, err := client.Models.GenerateImages(ctx, model, mediaPrompt(request.Prompt, request.StyleHint), &genai.GenerateImagesConfig{
		NumberOfImages: atLeastOne(request.Count),
		AspectRatio:    aspectRatio,
	})
	if err != nil {
		return nil, fmt.Errorf("genai GenerateImages: %w", err)
	}
	results := make([]domain.GeneratedMedia, 0, len(resp.GeneratedImages))
	for _, image := range resp.GeneratedImages {
		if image.Image != nil && len(image.Image.ImageBytes) > 0 {
			results = append(results, domain.GeneratedMedia{Data: image.Image.ImageBytes, MimeType: "image/png"})
		}
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("genai: no images generated")
	}
	return results, nil
}

func (p *Google) GenerateVideo(ctx context.Context, model string, request domain.VideoRequest) ([]domain.GeneratedMedia, error) {
	if model == "" {
		return nil, fmt.Errorf("%w: no video model configured", domain.ErrValidation)
	}
	client, err := p.newClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("genai.NewClient: %w", err)
	}
	duration := request.DurationSeconds
	if duration <= 0 {
		duration = 5
	}
	operation, err := client.Models.GenerateVideos(ctx, model, mediaPrompt(request.Prompt, request.StyleHint), nil, &genai.GenerateVideosConfig{
		DurationSeconds: genai.Ptr(duration),
		NumberOfVideos:  atLeastOne(request.Count),
	})
	if err != nil {
		return nil, fmt.Errorf("genai GenerateVideos: %w", err)
	}
	interval := p.VideoPollInterval
	if interval <= 0 {
		interval = time.Second
	}
	for !operation.Done {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
		if operation, err = client.Operations.GetVideosOperation(ctx, operation, nil); err != nil {
			return nil, fmt.Errorf("genai GetVideosOperation: %w", err)
		}
	}
	if operation.Error != nil {
		return nil, fmt.Errorf("genai video generation failed: %v", operation.Error)
	}
	if operation.Response == nil || len(operation.Response.GeneratedVideos) == 0 {
		return nil, fmt.Errorf("genai: no videos generated")
	}
	results := make([]domain.GeneratedMedia, 0, len(operation.Response.GeneratedVideos))
	for _, video := range operation.Response.GeneratedVideos {
		if video.Video != nil && len(video.Video.VideoBytes) > 0 {
			results = append(results, domain.GeneratedMedia{Data: video.Video.VideoBytes, MimeType: "video/mp4"})
		}
	}
	return results, nil
}
