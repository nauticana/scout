package runtime

import (
	"sync"

	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/service/controlplane"
)

// Registry hands out the long-lived Scout services bound to one database,
// caching each so the named-SQL QueryService behind it is compiled once.
// Downstream composition code holds a Registry instead of re-caching every
// service itself.
type Registry struct {
	db keelport.DatabaseRepository

	once      sync.Once
	resolver  *PublishedAgentResolver
	runs      *AgentRunStore
	opsEvents *AgentOpsEventStore
	deployed  *DeployedAgentIndex
	models    *controlplane.ModelCatalog
}

var registries sync.Map

// For returns the registry bound to db, creating it on first use. Safe for
// concurrent callers; repeated calls with the same repository share services.
func For(db keelport.DatabaseRepository) *Registry {
	if cached, ok := registries.Load(db); ok {
		return cached.(*Registry)
	}
	registry, _ := registries.LoadOrStore(db, &Registry{db: db})
	return registry.(*Registry)
}

func (r *Registry) init() {
	r.once.Do(func() {
		r.resolver = &PublishedAgentResolver{DB: r.db}
		r.runs = &AgentRunStore{DB: r.db}
		r.opsEvents = &AgentOpsEventStore{DB: r.db}
		r.deployed = &DeployedAgentIndex{DB: r.db}
		r.models = &controlplane.ModelCatalog{DB: r.db}
	})
}

func (r *Registry) Definitions() *PublishedAgentResolver {
	r.init()
	return r.resolver
}

func (r *Registry) Runs() *AgentRunStore {
	r.init()
	return r.runs
}

func (r *Registry) OpsEvents() *AgentOpsEventStore {
	r.init()
	return r.opsEvents
}

func (r *Registry) Deployed() *DeployedAgentIndex {
	r.init()
	return r.deployed
}

func (r *Registry) Models() *controlplane.ModelCatalog {
	r.init()
	return r.models
}
