package domain

// ModelRequest contains one tenant-scoped inference request.
type ModelRequest struct {
	TenantContext     TenantContext
	RequestID         string
	ConversationID    string
	ComplexitySignals map[string]float64
	Prompt            []byte
	MaxOutputTokens   int64
}

// ModelSelection identifies the chosen provider, model, and capacity pool.
type ModelSelection struct {
	Provider     string
	Model        string
	CapacityPool string
}

// ModelResult contains model output, termination reason, and usage.
type ModelResult struct {
	Output       []byte
	FinishReason string
	Usage        Usage
}

// ModelChunk is one ordered frame from a streaming model response.
type ModelChunk struct {
	Sequence     int64
	Payload      []byte
	FinishReason string
	Usage        Usage
}
