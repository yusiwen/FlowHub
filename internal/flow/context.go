package flow

import "github.com/yusiwen/flowhub/internal/flow/expr"

// schema is the static shape of one context value. dynamic means "any path below
// this point is allowed and has type Any"; the raw event payload and manual
// parameters use it.
type schema struct {
	typ      expr.Type
	children map[string]*schema
	dynamic  bool
}

func leaf(typ expr.Type) *schema {
	return &schema{typ: typ}
}

func objectNode(children map[string]*schema) *schema {
	return &schema{typ: expr.Object, children: children}
}

func dynamicNode() *schema {
	return &schema{typ: expr.Any, dynamic: true}
}

func (s *schema) resolve(path []string) (expr.Type, bool) {
	current := s
	for _, key := range path {
		if current.dynamic {
			return expr.Any, true
		}
		next, ok := current.children[key]
		if !ok {
			return expr.Any, false
		}
		current = next
	}
	if current.dynamic {
		return expr.Any, true
	}
	return current.typ, true
}

// contextResolver restricts the schema for one compilation site: a trigger
// condition sees only trigger, and a node or edge sees only the steps that are its
// ancestors.
type contextResolver struct {
	root  *schema
	only  string
	steps map[string]bool
}

func (r contextResolver) Resolve(path []string) (expr.Type, bool) {
	if len(path) == 0 {
		return expr.Any, false
	}
	if r.only != "" && path[0] != r.only {
		return expr.Any, false
	}
	if path[0] == "steps" {
		if len(path) < 2 {
			return expr.Any, false
		}
		if r.steps == nil || !r.steps[path[1]] {
			return expr.Any, false
		}
	}
	return r.root.resolve(path)
}

func buildContext(nodes map[string]*CompiledNode, tasks map[string]TaskSpec) *schema {
	steps := objectNode(map[string]*schema{})
	for id, node := range nodes {
		steps.children[id] = objectNode(map[string]*schema{
			"state":  leaf(expr.String),
			"output": objectNode(outputSchema(NodeType(node.Node.Type))),
		})
	}

	taskNodes := objectNode(map[string]*schema{})
	for name := range tasks {
		taskNodes.children[name] = objectNode(map[string]*schema{
			"state":       leaf(expr.String),
			"plan":        leaf(expr.String),
			"session":     leaf(expr.String),
			"worktree":    leaf(expr.String),
			"last_reply":  leaf(expr.String),
			"last_action": leaf(expr.String),
			"refusals":    leaf(expr.Array),
			"grant":       dynamicNode(),
		})
	}

	trigger := objectNode(map[string]*schema{
		"id":          leaf(expr.String),
		"schema":      leaf(expr.Number),
		"upstream":    leaf(expr.String),
		"kind":        leaf(expr.String),
		"occurred_at": leaf(expr.String),
		"received_at": leaf(expr.String),
		"subject": objectNode(map[string]*schema{
			"type":    leaf(expr.String),
			"project": leaf(expr.String),
			"key":     leaf(expr.String),
			"title":   leaf(expr.String),
			"url":     leaf(expr.String),
		}),
		"actor": objectNode(map[string]*schema{
			"login":   leaf(expr.String),
			"display": leaf(expr.String),
		}),
		"changes": leaf(expr.Array),
		"comment": objectNode(map[string]*schema{
			"text":   leaf(expr.String),
			"author": leaf(expr.String),
		}),
		"payload":    dynamicNode(),
		"dedupe_key": leaf(expr.String),
		"origin":     leaf(expr.String),
		"trace":      dynamicNode(),
	})

	return objectNode(map[string]*schema{
		"trigger": trigger,
		"run": objectNode(map[string]*schema{
			"id":         leaf(expr.String),
			"started_at": leaf(expr.String),
		}),
		"params": dynamicNode(),
		"steps":  steps,
		"tasks":  taskNodes,
	})
}

func outputSchema(typ NodeType) map[string]*schema {
	switch typ {
	case NodeEnsure:
		return map[string]*schema{
			"task_id":  leaf(expr.String),
			"routable": leaf(expr.Bool),
			"project":  leaf(expr.String),
			"runtime":  leaf(expr.String),
			"reused":   leaf(expr.Bool),
		}
	case NodeEvaluate:
		return map[string]*schema{
			"action": leaf(expr.String),
			"basis":  leaf(expr.String),
			"reason": leaf(expr.String),
		}
	case NodeTurn:
		return map[string]*schema{
			"session_id": leaf(expr.String),
			"reply":      leaf(expr.String),
			"cost":       leaf(expr.Number),
			"tokens":     leaf(expr.Number),
			"timed_out":  leaf(expr.Bool),
			"refusals":   leaf(expr.Array),
		}
	case NodeGrant:
		return map[string]*schema{
			"granted": leaf(expr.Array),
			"actor":   leaf(expr.String),
		}
	case NodeRevoke:
		return map[string]*schema{
			"revoked": leaf(expr.Bool),
		}
	case NodeBranch:
		return map[string]*schema{
			"selected": leaf(expr.String),
		}
	default:
		return map[string]*schema{}
	}
}
