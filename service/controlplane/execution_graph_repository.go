package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/nauticana/keel/common"
	keelport "github.com/nauticana/keel/port"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
)

const (
	qGraphInsert      = "scout_execution_graph_insert"
	qGraphGet         = "scout_execution_graph_get"
	qGraphStepInsert  = "scout_execution_step_insert"
	qGraphEntryInsert = "scout_execution_graph_entry_insert"
	qGraphEdgeInsert  = "scout_execution_transition_insert"
	qGraphSteps       = "scout_execution_graph_steps"
	qGraphEdges       = "scout_execution_graph_edges"

	executionGraphCatalogID = "scout.controlplane.execution_graph"
	// GraphCompilerVersion stamps graphs this package compiles.
	GraphCompilerVersion = "scout.tool_loop.v1"
	toolLoopStepID       = "tool_loop"
)

var executionGraphQueries = map[string]string{
	qGraphInsert: `
INSERT INTO execution_graph (tenant_id, agent_id, agent_version, graph_digest, compiler_version)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (tenant_id, agent_id, agent_version) DO NOTHING
RETURNING agent_version`,
	qGraphGet: `
SELECT g.graph_digest, s.step_id
  FROM execution_graph g
  JOIN execution_graph_entry e ON e.tenant_id = g.tenant_id AND e.agent_id = g.agent_id AND e.agent_version = g.agent_version
  JOIN execution_step s ON s.id = e.execution_step_id
 WHERE g.tenant_id = ? AND g.agent_id = ? AND g.agent_version = ?`,
	qGraphStepInsert: `
INSERT INTO execution_step (id, tenant_id, agent_id, agent_version, step_id, step_kind_code, configuration)
VALUES (nextval('execution_step_seq'), ?, ?, ?, ?, ?, ?)
RETURNING id`,
	qGraphEntryInsert: `
INSERT INTO execution_graph_entry (tenant_id, agent_id, agent_version, execution_step_id)
VALUES (?, ?, ?, ?)`,
	qGraphEdgeInsert: `
INSERT INTO execution_transition (source_step_id, transition_key, target_step_id)
VALUES (?, ?, ?)`,
	qGraphSteps: `
SELECT id, step_id, step_kind_code, configuration
  FROM execution_step
 WHERE tenant_id = ? AND agent_id = ? AND agent_version = ?
 ORDER BY id`,
	qGraphEdges: `
SELECT source.step_id, target.step_id
  FROM execution_transition t
  JOIN execution_step source ON source.id = t.source_step_id
  JOIN execution_step target ON target.id = t.target_step_id
 WHERE source.tenant_id = ? AND source.agent_id = ? AND source.agent_version = ?
 ORDER BY t.source_step_id, t.transition_key`,
}

// TableExecutionGraphRepository persists compiled graphs over execution_graph,
// execution_step, execution_graph_entry, and execution_transition. A graph is
// immutable: storing the same digest again is a no-op, another digest is ErrConflict.
type TableExecutionGraphRepository struct {
	DB keelport.DatabaseRepository
	// Compiler is required only by WriteRelease, which compiles at publication.
	Compiler contract.AgentCompiler

	once sync.Once
	qs   keelport.QueryService
}

func (repository *TableExecutionGraphRepository) init(ctx context.Context) error {
	if repository.DB == nil {
		return fmt.Errorf("execution graph repository: database is required")
	}
	repository.once.Do(func() { repository.qs = repository.DB.GetQueryService(ctx, executionGraphQueries) })
	if repository.qs == nil {
		return fmt.Errorf("execution graph repository: query service is required")
	}
	return nil
}

// Put stores one graph atomically.
func (repository *TableExecutionGraphRepository) Put(ctx context.Context, tenantID int64, graph domain.ExecutionGraph) error {
	if err := repository.init(ctx); err != nil {
		return err
	}
	tx, err := repository.DB.BeginTx(ctx, executionGraphQueries)
	if err != nil {
		return fmt.Errorf("begin graph publication: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	if err = putGraph(ctx, tx, tenantID, graph); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit graph publication: %w", err)
	}
	committed = true
	return nil
}

// WriteRelease compiles and stores the graph of an executable definition inside
// the caller's publish transaction. A definition without a tool loop has no graph.
func (repository *TableExecutionGraphRepository) WriteRelease(ctx context.Context, tx keelport.TxQueryService, tenantID int64, definition domain.AgentDefinition) error {
	if definition.ToolLoop == nil {
		return nil
	}
	if repository.Compiler == nil {
		return fmt.Errorf("%w: an executable definition needs a graph compiler", domain.ErrNotReady)
	}
	catalog, ok := tx.(keelport.TxQueryCatalog)
	if !ok {
		return fmt.Errorf("%w: the publish transaction cannot bind another query catalog", domain.ErrNotReady)
	}
	graph, err := repository.Compiler.Compile(ctx, definition)
	if err != nil {
		return fmt.Errorf("compile agent %q version %q: %w", definition.AgentID, definition.Version, err)
	}
	return putGraph(ctx, catalog.QueryService(executionGraphCatalogID, executionGraphQueries), tenantID, graph)
}

func putGraph(ctx context.Context, tx keelport.QueryService, tenantID int64, graph domain.ExecutionGraph) error {
	if err := validateGraph(tenantID, graph); err != nil {
		return err
	}
	inserted, err := tx.Query(ctx, qGraphInsert, tenantID, graph.AgentID, graph.Version, graph.Digest, GraphCompilerVersion)
	if err != nil {
		return fmt.Errorf("insert execution graph: %w", err)
	}
	if len(inserted.Rows) == 0 {
		stored, err := tx.Query(ctx, qGraphGet, tenantID, graph.AgentID, graph.Version)
		if err != nil {
			return fmt.Errorf("read execution graph: %w", err)
		}
		if len(stored.Rows) == 0 || common.AsString(stored.Rows[0][0]) != graph.Digest {
			return fmt.Errorf("%w: %s@%s already has a different execution graph", domain.ErrConflict, graph.AgentID, graph.Version)
		}
		return nil
	}
	ids := make(map[string]int64, len(graph.Steps))
	for _, step := range graph.Steps {
		row, err := tx.Query(ctx, qGraphStepInsert, tenantID, graph.AgentID, graph.Version, step.StepID, step.Kind, string(step.Configuration))
		if err != nil {
			return fmt.Errorf("insert execution step %q: %w", step.StepID, err)
		}
		if len(row.Rows) == 0 {
			return fmt.Errorf("insert execution step %q returned no id", step.StepID)
		}
		ids[step.StepID] = common.AsInt64(row.Rows[0][0])
	}
	if _, err = tx.Query(ctx, qGraphEntryInsert, tenantID, graph.AgentID, graph.Version, ids[graph.EntryStepID]); err != nil {
		return fmt.Errorf("insert graph entry: %w", err)
	}
	for _, step := range graph.Steps {
		for _, next := range step.NextStepIDs {
			if _, err = tx.Query(ctx, qGraphEdgeInsert, ids[step.StepID], next, ids[next]); err != nil {
				return fmt.Errorf("insert transition %s→%s: %w", step.StepID, next, err)
			}
		}
	}
	return nil
}

func validateGraph(tenantID int64, graph domain.ExecutionGraph) error {
	if tenantID <= 0 || strings.TrimSpace(graph.AgentID) == "" || strings.TrimSpace(graph.Version) == "" || !validSHA256(graph.Digest) || len(graph.Steps) == 0 {
		return fmt.Errorf("%w: a graph needs tenant, agent, version, digest, and steps", domain.ErrValidation)
	}
	known := make(map[string]struct{}, len(graph.Steps))
	for _, step := range graph.Steps {
		if strings.TrimSpace(step.StepID) == "" || strings.TrimSpace(step.Kind) == "" {
			return fmt.Errorf("%w: every execution step needs an id and a kind", domain.ErrValidation)
		}
		if _, duplicate := known[step.StepID]; duplicate {
			return fmt.Errorf("%w: execution step %q is declared twice", domain.ErrValidation, step.StepID)
		}
		known[step.StepID] = struct{}{}
	}
	if _, ok := known[graph.EntryStepID]; !ok {
		return fmt.Errorf("%w: entry step %q is not in the graph", domain.ErrValidation, graph.EntryStepID)
	}
	for _, step := range graph.Steps {
		for _, next := range step.NextStepIDs {
			if _, ok := known[next]; !ok {
				return fmt.Errorf("%w: step %q transitions to unknown step %q", domain.ErrValidation, step.StepID, next)
			}
		}
	}
	return nil
}

// Get returns the stored graph with each step's compiled ExecutionStepID.
func (repository *TableExecutionGraphRepository) Get(ctx context.Context, tenantID int64, agentID, version string) (domain.ExecutionGraph, error) {
	if err := repository.init(ctx); err != nil {
		return domain.ExecutionGraph{}, err
	}
	header, err := repository.qs.Query(ctx, qGraphGet, tenantID, agentID, version)
	if err != nil {
		return domain.ExecutionGraph{}, fmt.Errorf("get execution graph: %w", err)
	}
	if len(header.Rows) == 0 {
		return domain.ExecutionGraph{}, fmt.Errorf("%w: execution graph of %s@%s", domain.ErrNotFound, agentID, version)
	}
	graph := domain.ExecutionGraph{
		AgentID: agentID, Version: version,
		Digest: common.AsString(header.Rows[0][0]), EntryStepID: common.AsString(header.Rows[0][1]),
	}
	steps, err := repository.qs.Query(ctx, qGraphSteps, tenantID, agentID, version)
	if err != nil {
		return domain.ExecutionGraph{}, fmt.Errorf("list execution steps: %w", err)
	}
	edges, err := repository.qs.Query(ctx, qGraphEdges, tenantID, agentID, version)
	if err != nil {
		return domain.ExecutionGraph{}, fmt.Errorf("list execution transitions: %w", err)
	}
	next := make(map[string][]string, len(edges.Rows))
	for _, row := range edges.Rows {
		source := common.AsString(row[0])
		next[source] = append(next[source], common.AsString(row[1]))
	}
	for _, row := range steps.Rows {
		stepID := common.AsString(row[1])
		graph.Steps = append(graph.Steps, domain.ExecutionStep{
			ExecutionStepID: common.AsInt64(row[0]), StepID: stepID, Kind: common.AsString(row[2]),
			Configuration: []byte(common.AsString(row[3])), NextStepIDs: next[stepID],
		})
	}
	return graph, nil
}

// ToolLoopGraphCompiler compiles a definition that declares a tool loop into a
// one-step graph. Products with richer graphs supply their own AgentCompiler.
type ToolLoopGraphCompiler struct{}

func (ToolLoopGraphCompiler) Compile(_ context.Context, definition domain.AgentDefinition) (domain.ExecutionGraph, error) {
	if definition.ToolLoop == nil {
		return domain.ExecutionGraph{}, fmt.Errorf("%w: definition %q declares no tool loop", domain.ErrValidation, definition.AgentID)
	}
	if definition.ToolLoop.NextStepID != "" {
		return domain.ExecutionGraph{}, fmt.Errorf("%w: a one-step graph cannot name a next step", domain.ErrValidation)
	}
	if search := definition.ToolLoop.Search; search != nil && search.MaxSearches < 0 {
		return domain.ExecutionGraph{}, fmt.Errorf("%w: max searches cannot be negative", domain.ErrValidation)
	}
	configuration, err := json.Marshal(definition.ToolLoop)
	if err != nil {
		return domain.ExecutionGraph{}, fmt.Errorf("encode tool loop configuration: %w", err)
	}
	return domain.ExecutionGraph{
		AgentID: definition.AgentID, Version: definition.Version, EntryStepID: toolLoopStepID,
		Digest: sha256Hex(GraphCompilerVersion + "\n" + definition.DefinitionDigest + "\n" + string(configuration)),
		Steps:  []domain.ExecutionStep{{StepID: toolLoopStepID, Kind: domain.StepKindToolLoop, Configuration: configuration}},
	}, nil
}

var (
	_ contract.ExecutionGraphRepository = (*TableExecutionGraphRepository)(nil)
	_ contract.AgentCompiler            = ToolLoopGraphCompiler{}
)
