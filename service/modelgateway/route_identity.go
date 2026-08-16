package modelgateway

import (
	"strings"

	"github.com/nauticana/scout/domain"
)

// defaultBytesPerToken is the prompt-size heuristic used when no tokenizer is injected.
const defaultBytesPerToken = 4

// EstimatePromptTokens is the default byte-length heuristic for prompt work.
func EstimatePromptTokens(prompt []byte) int64 {
	if len(prompt) == 0 {
		return 0
	}
	return int64((len(prompt)-1)/defaultBytesPerToken + 1)
}

func promptTokens(estimate func([]byte) int64, prompt []byte) int64 {
	if estimate == nil {
		estimate = EstimatePromptTokens
	}
	return max(0, estimate(prompt))
}

// routeKey is the identity shared by candidates, snapshots, and selections.
type routeKey struct {
	Provider, Model, Region, RouteID string
}

func candidateRouteKey(candidate domain.ModelCandidate) routeKey {
	return routeKey{candidate.Provider, candidate.Model, candidate.Region, candidate.RouteID}
}

func snapshotRouteKey(snapshot domain.CapacitySnapshot) routeKey {
	return routeKey{snapshot.Provider, snapshot.Model, snapshot.Region, snapshot.RouteID}
}

func selectionRouteKey(selection domain.ModelSelection) routeKey {
	return routeKey{selection.Provider, selection.Model, selection.Region, selection.RouteID}
}

func (key routeKey) valid() bool {
	return strings.TrimSpace(key.Provider) != "" && strings.TrimSpace(key.Model) != ""
}

func (key routeKey) String() string {
	return key.Provider + "/" + key.Model + "@" + key.Region + "#" + key.RouteID
}
