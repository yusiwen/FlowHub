package flow

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/yusiwen/flowhub/internal/flow/expr"
)

var (
	flowIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,63}$`)
	namePattern   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
)

// Compile validates a definition and returns the graph the engine may execute.
// It reports the first problem it finds; a definition is never partially usable.
func Compile(definition Definition) (*Compiled, error) {
	flowID := definition.ID
	if definition.Version != 1 {
		return nil, fmt.Errorf("flow %q: version %d is not a definition this binary knows (it knows 1)", flowID, definition.Version)
	}
	if !flowIDPattern.MatchString(flowID) {
		return nil, fmt.Errorf("flow %q: id must match %s", flowID, flowIDPattern)
	}
	if strings.TrimSpace(definition.Name) == "" {
		return nil, fmt.Errorf("flow %q: name is required", flowID)
	}
	if strings.TrimSpace(definition.Upstream) == "" {
		return nil, fmt.Errorf("flow %q: upstream is required", flowID)
	}
	if len(definition.Triggers) == 0 {
		return nil, fmt.Errorf("flow %q: at least one trigger is required", flowID)
	}
	if len(definition.Nodes) == 0 {
		return nil, fmt.Errorf("flow %q: at least one node is required", flowID)
	}
	if len(definition.Edges) == 0 {
		return nil, fmt.Errorf("flow %q: at least one edge is required", flowID)
	}

	tasks := make(map[string]TaskSpec, len(definition.Tasks))
	for name, spec := range definition.Tasks {
		if !namePattern.MatchString(name) {
			return nil, fmt.Errorf("flow %q: task %q must match %s", flowID, name, namePattern)
		}
		if spec.Identity != "upstream" {
			return nil, fmt.Errorf("flow %q: task %q: identity %q is not supported (it is %q)", flowID, name, spec.Identity, "upstream")
		}
		tasks[name] = spec
	}

	nodes := make(map[string]*CompiledNode, len(definition.Nodes))
	declaration := make([]string, 0, len(definition.Nodes))
	for _, node := range definition.Nodes {
		if !namePattern.MatchString(node.ID) {
			return nil, fmt.Errorf("flow %q: node id %q must match %s", flowID, node.ID, namePattern)
		}
		if _, exists := nodes[node.ID]; exists {
			return nil, fmt.Errorf("flow %q: duplicate node id %q", flowID, node.ID)
		}
		config, err := decodeNodeConfig(node.Type, node.Config)
		if err != nil {
			return nil, fmt.Errorf("flow %q: node %q: %w", flowID, node.ID, err)
		}
		if ref := taskRefOf(config); ref != "" {
			if _, ok := tasks[ref]; !ok {
				return nil, fmt.Errorf("flow %q: node %q: task %q is not declared", flowID, node.ID, ref)
			}
		}
		nodes[node.ID] = &CompiledNode{
			Node:      node,
			Config:    config,
			Ancestors: map[string]bool{},
		}
		declaration = append(declaration, node.ID)
	}

	startID := ""
	ends := 0
	for _, id := range declaration {
		switch NodeType(nodes[id].Node.Type) {
		case NodeStart:
			if startID != "" {
				return nil, fmt.Errorf("flow %q: more than one start node", flowID)
			}
			startID = id
		case NodeEnd:
			ends++
		}
	}
	if startID == "" {
		return nil, fmt.Errorf("flow %q: a start node is required", flowID)
	}
	if ends == 0 {
		return nil, fmt.Errorf("flow %q: at least one end node is required", flowID)
	}

	edges := make([]*CompiledEdge, 0, len(definition.Edges))
	for _, edge := range definition.Edges {
		from, ok := nodes[edge.From]
		if !ok {
			return nil, fmt.Errorf("flow %q: edge %q -> %q: unknown source %q", flowID, edge.From, edge.To, edge.From)
		}
		to, ok := nodes[edge.To]
		if !ok {
			return nil, fmt.Errorf("flow %q: edge %q -> %q: unknown target %q", flowID, edge.From, edge.To, edge.To)
		}
		if edge.From == edge.To {
			return nil, fmt.Errorf("flow %q: edge %q -> %q is a self edge", flowID, edge.From, edge.To)
		}
		compiled := &CompiledEdge{Edge: edge}
		from.Next = append(from.Next, compiled)
		to.In = append(to.In, compiled)
		edges = append(edges, compiled)
	}

	for _, id := range declaration {
		node := nodes[id]
		switch NodeType(node.Node.Type) {
		case NodeStart:
			if len(node.In) != 0 {
				return nil, fmt.Errorf("flow %q: start %q must not have incoming edges", flowID, id)
			}
			if len(node.Next) != 1 {
				return nil, fmt.Errorf("flow %q: start %q must have exactly one outgoing edge", flowID, id)
			}
			if node.Next[0].Edge.When != "" {
				return nil, fmt.Errorf("flow %q: start %q must not have a condition", flowID, id)
			}
		case NodeEnd:
			if len(node.Next) != 0 {
				return nil, fmt.Errorf("flow %q: end %q must not have outgoing edges", flowID, id)
			}
		case NodeBranch:
			if len(node.Next) < 2 {
				return nil, fmt.Errorf("flow %q: branch %q needs at least one condition and one default", flowID, id)
			}
			defaults := 0
			for index, edge := range node.Next {
				if edge.Edge.When != "" {
					continue
				}
				defaults++
				if index != len(node.Next)-1 {
					return nil, fmt.Errorf("flow %q: branch %q: the default edge must be last", flowID, id)
				}
			}
			if defaults != 1 {
				return nil, fmt.Errorf("flow %q: branch %q needs exactly one default edge", flowID, id)
			}
		default:
			if len(node.Next) != 1 {
				return nil, fmt.Errorf("flow %q: node %q must have exactly one outgoing edge", flowID, id)
			}
			if node.Next[0].Edge.When != "" {
				return nil, fmt.Errorf("flow %q: node %q: only a branch may carry a condition", flowID, id)
			}
		}
	}

	order, err := topoOrder(nodes, declaration, flowID)
	if err != nil {
		return nil, err
	}
	if err := checkReachable(nodes, startID, flowID); err != nil {
		return nil, err
	}

	for _, id := range order {
		node := nodes[id]
		for _, edge := range node.In {
			from := nodes[edge.Edge.From]
			node.Ancestors[from.Node.ID] = true
			for ancestor := range from.Ancestors {
				node.Ancestors[ancestor] = true
			}
		}
	}

	root := buildContext(nodes, tasks)

	triggers := make([]CompiledTrigger, 0, len(definition.Triggers))
	seenTriggers := make(map[string]bool, len(definition.Triggers))
	for _, trigger := range definition.Triggers {
		if !namePattern.MatchString(trigger.ID) {
			return nil, fmt.Errorf("flow %q: trigger id %q must match %s", flowID, trigger.ID, namePattern)
		}
		if seenTriggers[trigger.ID] {
			return nil, fmt.Errorf("flow %q: duplicate trigger id %q", flowID, trigger.ID)
		}
		seenTriggers[trigger.ID] = true
		if strings.TrimSpace(trigger.Event) == "" || strings.ContainsAny(trigger.Event, " \t\r\n") {
			return nil, fmt.Errorf("flow %q: trigger %q: event is required and must not contain whitespace", flowID, trigger.ID)
		}
		scope := trigger.Dedupe.Scope
		if scope == "" {
			scope = "event"
		}
		switch scope {
		case "event", "subject", "none":
		default:
			return nil, fmt.Errorf("flow %q: trigger %q: dedupe scope %q is not one of event, subject, none", flowID, trigger.ID, scope)
		}
		trigger.Dedupe.Scope = scope
		compiledTrigger := CompiledTrigger{Trigger: trigger}
		if trigger.When != "" {
			program, err := expr.Compile(trigger.When, contextResolver{root: root, only: "trigger"})
			if err != nil {
				return nil, fmt.Errorf("flow %q: trigger %q: %w", flowID, trigger.ID, err)
			}
			if program.Type() != expr.Bool {
				return nil, fmt.Errorf("flow %q: trigger %q: condition must be bool, got %s", flowID, trigger.ID, program.Type())
			}
			compiledTrigger.When = program
		}
		triggers = append(triggers, compiledTrigger)
	}

	limits, err := compileLimits(definition.Limits)
	if err != nil {
		return nil, fmt.Errorf("flow %q: %w", flowID, err)
	}

	for _, id := range declaration {
		node := nodes[id]
		turn, ok := node.Config.(*TurnConfig)
		if !ok || turn.Prompt == "" {
			continue
		}
		template, err := expr.CompileTemplate(turn.Prompt, contextResolver{root: root, steps: copyAncestors(node.Ancestors)})
		if err != nil {
			return nil, fmt.Errorf("flow %q: node %q: prompt: %w", flowID, id, err)
		}
		turn.Template = template
	}

	for _, edge := range edges {
		if edge.Edge.When == "" {
			continue
		}
		from := nodes[edge.Edge.From]
		program, err := expr.Compile(edge.Edge.When, contextResolver{root: root, steps: copyAncestors(from.Ancestors)})
		if err != nil {
			return nil, fmt.Errorf("flow %q: edge %q -> %q: %w", flowID, edge.Edge.From, edge.Edge.To, err)
		}
		if program.Type() != expr.Bool {
			return nil, fmt.Errorf("flow %q: edge %q -> %q: condition must be bool, got %s", flowID, edge.Edge.From, edge.Edge.To, program.Type())
		}
		edge.When = program
	}

	return &Compiled{
		Definition: definition,
		Nodes:      nodes,
		Order:      order,
		Start:      startID,
		Limits:     limits,
		Triggers:   triggers,
	}, nil
}

func topoOrder(nodes map[string]*CompiledNode, declaration []string, flowID string) ([]string, error) {
	indegree := make(map[string]int, len(nodes))
	for id := range nodes {
		indegree[id] = len(nodes[id].In)
	}
	queue := make([]string, 0, len(nodes))
	for _, id := range declaration {
		if indegree[id] == 0 {
			queue = append(queue, id)
		}
	}
	order := make([]string, 0, len(nodes))
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		order = append(order, id)
		for _, edge := range nodes[id].Next {
			indegree[edge.Edge.To]--
			if indegree[edge.Edge.To] == 0 {
				queue = append(queue, edge.Edge.To)
			}
		}
	}
	if len(order) != len(nodes) {
		return nil, fmt.Errorf("flow %q: the graph has a cycle", flowID)
	}
	return order, nil
}

func checkReachable(nodes map[string]*CompiledNode, start, flowID string) error {
	seen := map[string]bool{start: true}
	queue := []string{start}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, edge := range nodes[id].Next {
			if !seen[edge.Edge.To] {
				seen[edge.Edge.To] = true
				queue = append(queue, edge.Edge.To)
			}
		}
	}
	if len(seen) == len(nodes) {
		return nil
	}
	var unreachable []string
	for id := range nodes {
		if !seen[id] {
			unreachable = append(unreachable, id)
		}
	}
	sort.Strings(unreachable)
	return fmt.Errorf("flow %q: unreachable nodes: %s", flowID, strings.Join(unreachable, ", "))
}

func compileLimits(limits *Limits) (Constraints, error) {
	var constraints Constraints
	if limits == nil {
		return constraints, nil
	}
	if limits.Timeout != "" {
		timeout, err := time.ParseDuration(limits.Timeout)
		if err != nil {
			return Constraints{}, fmt.Errorf("limits.timeout: %w", err)
		}
		if timeout < 0 {
			return Constraints{}, fmt.Errorf("limits.timeout must not be negative")
		}
		constraints.Timeout = timeout
	}
	if limits.MaxCost < 0 {
		return Constraints{}, fmt.Errorf("limits.max_cost must not be negative")
	}
	constraints.MaxCost = limits.MaxCost
	return constraints, nil
}

func copyAncestors(ancestors map[string]bool) map[string]bool {
	copy := make(map[string]bool, len(ancestors))
	for id := range ancestors {
		copy[id] = true
	}
	return copy
}
