package charter

import (
	"encoding/json"
	"fmt"

	"github.com/nauticana/charter/sdk/model"

	"github.com/nauticana/scout/domain"
)

// ExtensionKey namespaces Scout's execution fields inside a Charter AgentDefinition (CHR-CONF-002).
const ExtensionKey = "scout:definition"

// Embed keeps the Charter definition authoritative and stores Scout's models, prompts and policies as its extension.
func Embed(def model.AgentDefinition, scout domain.AgentDefinition) (model.AgentDefinition, error) {
	raw, err := json.Marshal(scout)
	if err != nil {
		return model.AgentDefinition{}, err
	}
	if def.Extensions == nil {
		def.Extensions = map[string]json.RawMessage{}
	}
	def.Extensions[ExtensionKey] = raw
	return def, nil
}

// Extract returns the Scout definition a Charter definition carries; ok is false when it carries none.
func Extract(def model.AgentDefinition) (scout domain.AgentDefinition, ok bool, err error) {
	raw, ok := def.Extensions[ExtensionKey]
	if !ok {
		return domain.AgentDefinition{}, false, nil
	}
	if err := json.Unmarshal(raw, &scout); err != nil {
		return domain.AgentDefinition{}, true, fmt.Errorf("charter definition %s: %s: %w", def.ID, ExtensionKey, err)
	}
	return scout, true, nil
}
