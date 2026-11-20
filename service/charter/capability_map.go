package charter

import "context"

// CapabilityMap is Capabilities fixed at composition, the same for every tenant.
type CapabilityMap map[string]Capability

var _ Capabilities = CapabilityMap(nil)

func (m CapabilityMap) Capability(_ context.Context, _ int64, toolID string) (Capability, bool, error) {
	target, found := m[toolID]
	return target, found, nil
}
