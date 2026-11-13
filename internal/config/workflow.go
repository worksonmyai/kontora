package config

import (
	"bytes"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

type PipelineDefinition struct {
	Legacy Pipeline
	Graph  *WorkflowGraph
}

func (p PipelineDefinition) IsGraph() bool { return p.Graph != nil }

func (p *PipelineDefinition) UnmarshalYAML(value *yaml.Node) error {
	*p = PipelineDefinition{}
	switch value.Kind {
	case yaml.SequenceNode:
		if err := validateLegacyFields(value); err != nil {
			return err
		}
		return value.Decode(&p.Legacy)
	case yaml.MappingNode:
		p.Graph = new(WorkflowGraph)
		return value.Decode(p.Graph)
	case yaml.DocumentNode, yaml.ScalarNode, yaml.AliasNode:
	}
	return fmt.Errorf("pipeline on line %d: expected a legacy list or workflow graph", value.Line)
}

func (p PipelineDefinition) MarshalYAML() (any, error) {
	if p.IsGraph() {
		if p.Legacy != nil {
			return nil, fmt.Errorf("pipeline cannot contain both legacy stages and a graph")
		}
		return p.Graph, nil
	}
	return p.Legacy, nil
}

// Node.Decode does not inherit KnownFields from the configuration decoder.
func validateLegacyFields(node *yaml.Node) error {
	return validateLegacyNodeFields(node, make(map[*yaml.Node]bool))
}

func validateLegacyNodeFields(node *yaml.Node, seen map[*yaml.Node]bool) error {
	if seen[node] {
		return nil
	}
	seen[node] = true
	if node.Kind == yaml.AliasNode {
		return validateLegacyNodeFields(node.Alias, seen)
	}
	if node.Kind == yaml.SequenceNode {
		for _, child := range node.Content {
			if err := validateLegacyNodeFields(child, seen); err != nil {
				return err
			}
		}
	}
	if node.Kind == yaml.MappingNode {
		for i := 0; i < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if key.Tag == "!!merge" {
				if err := validateLegacyNodeFields(value, seen); err != nil {
					return err
				}
				continue
			}
			switch key.Value {
			case "stage", "agent", "on_success", "on_failure", "max_retries", "running_status":
			default:
				return fmt.Errorf("line %d: field %s not found in type config.PipelineStep", key.Line, key.Value)
			}
		}
	}
	return nil
}

type WorkflowGraph struct {
	Version int            `yaml:"version"`
	Entry   string         `yaml:"entry"`
	Nodes   []WorkflowNode `yaml:"nodes"`
	Edges   []WorkflowEdge `yaml:"edges"`
}

type WorkflowNode struct {
	ID       string             `yaml:"id"`
	Type     string             `yaml:"type"`
	Stage    string             `yaml:"stage,omitempty"`
	Agent    string             `yaml:"agent,omitempty"`
	Verdicts []string           `yaml:"verdicts,omitempty"`
	Command  string             `yaml:"command,omitempty"`
	Timeout  Duration           `yaml:"timeout,omitempty"`
	When     *WorkflowCondition `yaml:"when,omitempty"`
	Max      int                `yaml:"max,omitempty"`
	Status   string             `yaml:"status,omitempty"`
}

type WorkflowCondition struct {
	Field  string `yaml:"field"`
	Equals any    `yaml:"equals"`
}

type WorkflowEdge struct {
	From WorkflowPort `yaml:"from"`
	To   string       `yaml:"to"`
}

type WorkflowPort struct {
	Node string `yaml:"node"`
	Port string `yaml:"port"`
}

func (g *WorkflowGraph) UnmarshalYAML(value *yaml.Node) error {
	if err := validateGraphYAML(value); err != nil {
		return err
	}
	type plain WorkflowGraph
	return strictWorkflowDecode(value, (*plain)(g))
}

func (n *WorkflowNode) UnmarshalYAML(value *yaml.Node) error {
	type plain WorkflowNode
	if err := strictWorkflowDecode(value, (*plain)(n)); err != nil {
		return err
	}
	allowed := map[string]bool{"id": true, "type": true}
	var fields []string
	switch n.Type {
	case "agent":
		fields = []string{"stage", "agent", "verdicts"}
	case "check":
		fields = []string{"command", "timeout"}
	case "condition":
		fields = []string{"when"}
	case "loop":
		fields = []string{"max"}
	case "end":
		fields = []string{"status"}
	default:
		return fmt.Errorf("node %q: unknown type %q", n.ID, n.Type)
	}
	for _, field := range fields {
		allowed[field] = true
	}
	for i := 0; i < len(value.Content); i += 2 {
		if key := value.Content[i].Value; !allowed[key] {
			return fmt.Errorf("node %q: field %q is not supported for type %q", n.ID, key, n.Type)
		}
	}
	return nil
}

func strictWorkflowDecode(value *yaml.Node, out any) error {
	data, err := yaml.Marshal(value)
	if err != nil {
		return err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	return decoder.Decode(out)
}

func validateGraphYAML(node *yaml.Node) error {
	if node.Kind == yaml.AliasNode {
		return fmt.Errorf("workflow graph on line %d: aliases are not supported", node.Line)
	}
	if node.Kind == yaml.MappingNode {
		seen := make(map[string]bool)
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Tag == "!!merge" || key.Value == "<<" {
				return fmt.Errorf("workflow graph on line %d: merge keys are not supported", key.Line)
			}
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return fmt.Errorf("workflow graph on line %d: field names must be strings", key.Line)
			}
			if seen[key.Value] {
				return fmt.Errorf("workflow graph on line %d: duplicate field %q", key.Line, key.Value)
			}
			seen[key.Value] = true
		}
	}
	for _, child := range node.Content {
		if err := validateGraphYAML(child); err != nil {
			return err
		}
	}
	return nil
}

// YAML resolves aliases before calling UnmarshalYAML, including aliases of whole graphs.
func validateGraphAliases(document *yaml.Node) error {
	type location struct {
		node      *yaml.Node
		pipelines bool
		inherited bool
	}
	seen := make(map[location]bool)
	var visit func(location) error
	visit = func(at location) error {
		if seen[at] {
			return nil
		}
		seen[at] = true
		node := at.node
		switch node.Kind {
		case yaml.ScalarNode:
		case yaml.AliasNode:
			return visit(location{node.Alias, at.pipelines, true})
		case yaml.DocumentNode, yaml.SequenceNode:
			for _, child := range node.Content {
				if err := visit(location{child, at.pipelines, at.inherited}); err != nil {
					return err
				}
			}
		case yaml.MappingNode:
			for i := 0; i < len(node.Content); i += 2 {
				key, value := node.Content[i], node.Content[i+1]
				if key.Tag == "!!merge" {
					if err := visit(location{value, at.pipelines, true}); err != nil {
						return err
					}
				} else if !at.pipelines && key.Value == "pipelines" {
					if err := visit(location{value, true, at.inherited}); err != nil {
						return err
					}
				} else if at.pipelines {
					aliased := at.inherited
					aliases := make(map[*yaml.Node]bool)
					for value.Kind == yaml.AliasNode {
						if aliases[value] {
							return fmt.Errorf("pipeline on line %d: recursive alias", value.Line)
						}
						aliases[value], aliased = true, true
						value = value.Alias
					}
					if value.Kind == yaml.MappingNode {
						if aliased {
							return fmt.Errorf("workflow graph on line %d: aliases and merge keys are not supported", value.Line)
						}
						if err := validateGraphYAML(value); err != nil {
							return err
						}
					}
				}
			}
		}
		return nil
	}
	return visit(location{node: document})
}

func (g WorkflowGraph) Node(id string) (WorkflowNode, bool) {
	for _, node := range g.Nodes {
		if node.ID == id {
			return node, true
		}
	}
	return WorkflowNode{}, false
}

func (g WorkflowGraph) Target(id, port string) (string, bool) {
	for _, edge := range g.Edges {
		if edge.From.Node == id && edge.From.Port == port {
			return edge.To, true
		}
	}
	return "", false
}

type ResolvedNode struct {
	ID       string
	Kind     string
	Template string
	Agent    string
}

func (p PipelineDefinition) Resolve(nodeID, override string) (ResolvedNode, error) {
	if p.IsGraph() {
		node, ok := p.Graph.Node(nodeID)
		if !ok {
			return ResolvedNode{}, fmt.Errorf("unknown workflow node %q", nodeID)
		}
		resolved := ResolvedNode{ID: node.ID, Kind: node.Type}
		if node.Type == "agent" {
			resolved.Template, resolved.Agent = node.Stage, node.Agent
			if override != "" {
				resolved.Agent = override
			}
		}
		return resolved, nil
	}
	for _, step := range p.Legacy {
		if step.Stage == nodeID {
			agent := step.Agent
			if override != "" {
				agent = override
			}
			return ResolvedNode{ID: nodeID, Kind: "agent", Template: step.Stage, Agent: agent}, nil
		}
	}
	return ResolvedNode{}, fmt.Errorf("unknown pipeline stage %q", nodeID)
}

var workflowIDPattern = regexp.MustCompile(`^[a-z0-9_-]+$`)

func workflowPorts(kind string) []string {
	switch kind {
	case "agent":
		return []string{"complete", "failed"}
	case "check":
		return []string{"pass", "fail"}
	case "condition":
		return []string{"true", "false"}
	case "loop":
		return []string{"repeat", "exhausted"}
	default:
		return nil
	}
}

func (c *Config) validateWorkflow(g *WorkflowGraph) error {
	if g.Version != 2 {
		return fmt.Errorf("unsupported workflow version %d (must be 2)", g.Version)
	}
	if len(g.Nodes) == 0 {
		return fmt.Errorf("workflow must have at least one node")
	}
	nodes, err := c.validateWorkflowNodes(g)
	if err != nil {
		return err
	}
	if _, ok := nodes[g.Entry]; !ok {
		return fmt.Errorf("entry %q: unknown node", g.Entry)
	}
	adjacent, err := validateWorkflowEdges(g, nodes)
	if err != nil {
		return err
	}
	if err := validateWorkflowReachability(g, adjacent); err != nil {
		return err
	}
	return validateWorkflowCycles(g, nodes, adjacent)
}

func (c *Config) validateWorkflowNodes(g *WorkflowGraph) (map[string]WorkflowNode, error) {
	nodes := make(map[string]WorkflowNode, len(g.Nodes))
	for _, node := range g.Nodes {
		if !workflowIDPattern.MatchString(node.ID) || slices.Contains([]string{"default", "annotation", "rework"}, node.ID) || strings.HasSuffix(node.ID, "-annotation") {
			return nil, fmt.Errorf("invalid or reserved workflow node ID %q", node.ID)
		}
		if _, exists := nodes[node.ID]; exists {
			return nil, fmt.Errorf("duplicate workflow node ID %q", node.ID)
		}
		nodes[node.ID] = node
		if err := c.validateWorkflowNode(node); err != nil {
			return nil, fmt.Errorf("node %q: %w", node.ID, err)
		}
	}
	return nodes, nil
}

func validateWorkflowEdges(g *WorkflowGraph, nodes map[string]WorkflowNode) (map[string][]WorkflowEdge, error) {
	edges := make(map[WorkflowPort]string, len(g.Edges))
	adjacent := make(map[string][]WorkflowEdge)
	for _, edge := range g.Edges {
		from, ok := nodes[edge.From.Node]
		if !ok {
			return nil, fmt.Errorf("edge source %q: unknown node", edge.From.Node)
		}
		if !slices.Contains(workflowPorts(from.Type), edge.From.Port) {
			return nil, fmt.Errorf("node %q: unknown port %q", from.ID, edge.From.Port)
		}
		if _, ok := nodes[edge.To]; !ok {
			return nil, fmt.Errorf("edge target %q: unknown node", edge.To)
		}
		if _, exists := edges[edge.From]; exists {
			return nil, fmt.Errorf("node %q port %q: duplicate edge", from.ID, edge.From.Port)
		}
		edges[edge.From] = edge.To
		adjacent[from.ID] = append(adjacent[from.ID], edge)
	}
	for _, node := range g.Nodes {
		for _, port := range workflowPorts(node.Type) {
			if _, ok := edges[WorkflowPort{Node: node.ID, Port: port}]; !ok {
				return nil, fmt.Errorf("node %q port %q: missing destination", node.ID, port)
			}
		}
	}
	return adjacent, nil
}

func validateWorkflowReachability(g *WorkflowGraph, adjacent map[string][]WorkflowEdge) error {
	visited := make(map[string]bool)
	var visit func(string)
	visit = func(id string) {
		if visited[id] {
			return
		}
		visited[id] = true
		for _, edge := range adjacent[id] {
			visit(edge.To)
		}
	}
	visit(g.Entry)
	for _, node := range g.Nodes {
		if !visited[node.ID] {
			return fmt.Errorf("unreachable node %q", node.ID)
		}
	}
	return nil
}

func validateWorkflowCycles(g *WorkflowGraph, nodes map[string]WorkflowNode, adjacent map[string][]WorkflowEdge) error {
	for _, controlOnly := range []bool{false, true} {
		colors := make(map[string]int)
		var cyclic func(string) bool
		cyclic = func(id string) bool {
			if colors[id] == 1 {
				return true
			}
			if colors[id] == 2 {
				return false
			}
			colors[id] = 1
			for _, edge := range adjacent[id] {
				if controlOnly {
					if executableWorkflowNode(nodes[id]) || executableWorkflowNode(nodes[edge.To]) {
						continue
					}
				} else if nodes[id].Type == "loop" && edge.From.Port == "repeat" {
					continue
				}
				if cyclic(edge.To) {
					return true
				}
			}
			colors[id] = 2
			return false
		}
		for _, node := range g.Nodes {
			if cyclic(node.ID) {
				if controlOnly {
					return fmt.Errorf("control-only workflow cycle at node %q", node.ID)
				}
				return fmt.Errorf("workflow cycle bypasses loop.repeat at node %q", node.ID)
			}
		}
	}
	return nil
}

func executableWorkflowNode(node WorkflowNode) bool {
	return node.Type == "agent" || node.Type == "check"
}

func validateWorkflowNodeFields(node WorkflowNode) error {
	// In-memory definitions need the same kind restrictions as decoded YAML.
	if node.Type != "agent" && (node.Stage != "" || node.Agent != "" || len(node.Verdicts) != 0) ||
		node.Type != "check" && (node.Command != "" || node.Timeout.Duration != 0) ||
		node.Type != "condition" && node.When != nil || node.Type != "loop" && node.Max != 0 ||
		node.Type != "end" && node.Status != "" {
		return fmt.Errorf("fields are not supported for type %q", node.Type)
	}
	return nil
}

func (c *Config) validateWorkflowNode(node WorkflowNode) error {
	if err := validateWorkflowNodeFields(node); err != nil {
		return err
	}
	switch node.Type {
	case "agent":
		return c.validateWorkflowAgent(node)
	case "check":
		if strings.TrimSpace(node.Command) == "" {
			return fmt.Errorf("command is required")
		}
		if node.Timeout.Duration <= 0 {
			return fmt.Errorf("timeout must be positive")
		}
	case "condition":
		return validateWorkflowCondition(node.When)
	case "loop":
		if node.Max <= 0 {
			return fmt.Errorf("max must be positive")
		}
	case "end":
		return c.validateWorkflowEndStatus(node.Status)
	default:
		return fmt.Errorf("unknown type %q", node.Type)
	}
	return nil
}

func (c *Config) validateWorkflowAgent(node WorkflowNode) error {
	stage, ok := c.Stages[node.Stage]
	if !ok {
		return fmt.Errorf("unknown stage %q", node.Stage)
	}
	agent, ok := c.Agents[node.Agent]
	if !ok {
		return fmt.Errorf("unknown agent %q", node.Agent)
	}
	model, effort := stage.Model.For(node.Agent, agent), stage.Effort.For(node.Agent, agent)
	if agent.Kind() == "" && (model != "" || effort != "") {
		return fmt.Errorf("stage %q sets model or effort unsupported by agent %q", node.Stage, node.Agent)
	}
	if err := agent.CheckEffort(model, effort); err != nil {
		return err
	}
	seen := make(map[string]bool)
	for _, verdict := range node.Verdicts {
		if strings.ContainsRune(verdict, '\x00') {
			return fmt.Errorf("verdict %q contains NUL", verdict)
		}
		if strings.TrimSpace(verdict) == "" || seen[verdict] {
			return fmt.Errorf("empty or duplicate verdict %q", verdict)
		}
		seen[verdict] = true
	}
	return nil
}

func validateWorkflowCondition(when *WorkflowCondition) error {
	if when == nil {
		return fmt.Errorf("when is required")
	}
	switch when.Field {
	case "verdict":
		if value, ok := when.Equals.(string); !ok || strings.TrimSpace(value) == "" {
			return fmt.Errorf("verdict equals must be a nonempty string")
		}
	case "exit_code":
		if _, ok := when.Equals.(int); !ok {
			return fmt.Errorf("exit_code equals must be an integer")
		}
	default:
		return fmt.Errorf("unknown condition field %q", when.Field)
	}
	return nil
}

func (c *Config) validateWorkflowEndStatus(status string) error {
	if status == "done" || status == "human_review" || status == "paused" {
		return nil
	}
	if !c.IsCustomStatus(status) || status == "closed" {
		return fmt.Errorf("invalid end status %q: use done, human_review, paused, or a custom parked status", status)
	}
	for _, definition := range c.Pipelines {
		for _, step := range definition.Legacy {
			if step.EffectiveRunningStatus() == status {
				return fmt.Errorf("end status %q is an active running_status, not a parked status", status)
			}
		}
	}
	return nil
}
