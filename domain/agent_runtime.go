package domain

// AgentTask is one request executed against a published agent definition. The
// compiled prompt sections supply the agent's standing configuration; these
// fields supply what is specific to this invocation.
type AgentTask struct {
	Task            string
	Context         string
	InputData       string
	OutputFormat    string
	PastPerformance string
}

// ImageRequest bounds one image generation call.
type ImageRequest struct {
	Prompt      string
	Count       int32
	AspectRatio string
	StyleHint   string
}

// VideoRequest bounds one video generation call.
type VideoRequest struct {
	Prompt          string
	Count           int32
	DurationSeconds int32
	StyleHint       string
}

// GeneratedMedia is one produced image or video with its content type.
type GeneratedMedia struct {
	Data     []byte
	MimeType string
}
