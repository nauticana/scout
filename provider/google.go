package provider

import (
	"context"
	"encoding/json"
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
	// Temperature is sent only when set; nil leaves sampling to the model.
	Temperature *float64
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
	contents, config, err := p.contentParams(request)
	if err != nil {
		return domain.ModelResult{}, err
	}
	resp, err := client.Models.GenerateContent(ctx, selection.Model, contents, config)
	if err != nil {
		return domain.ModelResult{}, fmt.Errorf("genai GenerateContent: %w", err)
	}
	return googleResult(resp, request)
}

func (p *Google) contentParams(request domain.ModelRequest) ([]*genai.Content, *genai.GenerateContentConfig, error) {
	if err := checkOutputMode(GoogleProviderID, request.Output); err != nil {
		return nil, nil, err
	}
	config := &genai.GenerateContentConfig{MaxOutputTokens: int32(maxOutputTokens(request))}
	if p.Temperature != nil {
		config.Temperature = genai.Ptr(float32(*p.Temperature))
	}
	if len(request.Tools) > 0 {
		declarations := make([]*genai.FunctionDeclaration, 0, len(request.Tools))
		for _, tool := range request.Tools {
			schema, err := schemaObject(tool.InputSchema)
			if err != nil {
				return nil, nil, fmt.Errorf("tool %q: %w", tool.Name, err)
			}
			declarations = append(declarations, &genai.FunctionDeclaration{Name: tool.Name, Description: tool.Description, ParametersJsonSchema: schema})
		}
		config.Tools = []*genai.Tool{{FunctionDeclarations: declarations}}
	}
	if request.Output.Mode == domain.OutputModeJSONSchema {
		schema, err := schemaObject(request.Output.Schema)
		if err != nil {
			return nil, nil, err
		}
		config.ResponseMIMEType = "application/json"
		config.ResponseJsonSchema = schema
	}
	contents := make([]*genai.Content, 0, len(request.Messages)+1)
	for _, message := range conversation(request) {
		content, err := googleContent(message)
		if err != nil {
			return nil, nil, err
		}
		contents = append(contents, content)
	}
	return contents, config, nil
}

func googleContent(message domain.ModelMessage) (*genai.Content, error) {
	parts := make([]*genai.Part, 0, 1+len(message.ToolCalls)+len(message.Observations))
	if len(message.Text) > 0 {
		parts = append(parts, genai.NewPartFromText(string(message.Text)))
	}
	for _, call := range message.ToolCalls {
		var args map[string]any
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			return nil, fmt.Errorf("%w: arguments of tool call %q: %w", domain.ErrValidation, call.CallID, err)
		}
		part := genai.NewPartFromFunctionCall(call.Name, args)
		part.FunctionCall.ID = call.CallID
		parts = append(parts, part)
	}
	for _, observation := range message.Observations {
		part := genai.NewPartFromFunctionResponse(observation.Name, googleObservation(observation))
		part.FunctionResponse.ID = observation.CallID
		parts = append(parts, part)
	}
	if message.Role == domain.ModelRoleAssistant {
		return genai.NewContentFromParts(parts, genai.RoleModel), nil
	}
	return genai.NewContentFromParts(parts, genai.RoleUser), nil
}

// googleObservation wraps output in the object Gemini requires: "output" on
// success, "error" on failure, with JSON output passed through structured.
func googleObservation(observation domain.ModelToolObservation) map[string]any {
	key := "output"
	if observation.IsError {
		key = "error"
	}
	var decoded any
	if json.Unmarshal(observation.Output, &decoded) != nil {
		decoded = string(observation.Output)
	}
	return map[string]any{key: decoded}
}

func googleResult(resp *genai.GenerateContentResponse, request domain.ModelRequest) (domain.ModelResult, error) {
	var calls []domain.ModelToolCall
	turnNo := 1
	for _, message := range request.Messages {
		if message.Role == domain.ModelRoleAssistant {
			turnNo++
		}
	}
	for index, call := range resp.FunctionCalls() {
		arguments, err := json.Marshal(call.Args)
		if err != nil {
			return domain.ModelResult{}, fmt.Errorf("genai: encode arguments of %q: %w", call.Name, err)
		}
		if call.Args == nil {
			arguments = []byte("{}")
		}
		callID := call.ID
		if callID == "" {
			// Gemini omits ids outside Vertex. Include the assistant turn so ids
			// remain distinct across a multi-iteration tool conversation.
			callID = fmt.Sprintf("call_%d_%d", turnNo, index+1)
		}
		calls = append(calls, domain.ModelToolCall{CallID: callID, Name: call.Name, Arguments: arguments})
	}
	text := ""
	if len(resp.Candidates) > 0 && resp.Candidates[0].Content != nil {
		for _, part := range resp.Candidates[0].Content.Parts {
			if part != nil && !part.Thought {
				text += part.Text
			}
		}
	}
	if err := emptyResult(GoogleProviderID, text, calls); err != nil {
		return domain.ModelResult{}, err
	}
	native := ""
	if len(resp.Candidates) > 0 {
		native = string(resp.Candidates[0].FinishReason)
	}
	usage := domain.Usage{}
	if resp.UsageMetadata != nil {
		usage.InputTokens = int64(resp.UsageMetadata.PromptTokenCount)
		usage.OutputTokens = int64(resp.UsageMetadata.CandidatesTokenCount)
	} else {
		// Vertex omits usage on some model families; a 4-chars-per-token
		// estimate keeps accounting non-zero rather than silently free.
		usage.InputTokens = int64(len(request.Prompt)) / 4
		usage.OutputTokens = int64(len(text)) / 4
	}
	return domain.ModelResult{Output: []byte(text), ToolCalls: calls, FinishReason: finishReason(native, calls), Usage: usage}, nil
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
