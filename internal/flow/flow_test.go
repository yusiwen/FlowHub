package flow

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func fixtureDefinition(t *testing.T) Definition {
	t.Helper()
	data, err := os.ReadFile("../../testdata/flows/default-youtrack.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	definition, err := Decode(data)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return definition
}

func cloneDefinition(t *testing.T, definition Definition) Definition {
	t.Helper()
	data, err := json.Marshal(definition)
	if err != nil {
		t.Fatalf("marshal definition: %v", err)
	}
	copy, err := Decode(data)
	if err != nil {
		t.Fatalf("decode clone: %v", err)
	}
	return copy
}

func nodeByID(t *testing.T, definition *Definition, id string) *Node {
	t.Helper()
	for index := range definition.Nodes {
		if definition.Nodes[index].ID == id {
			return &definition.Nodes[index]
		}
	}
	t.Fatalf("node %q not found", id)
	return nil
}

func edgeBetween(t *testing.T, definition *Definition, from, to string) *Edge {
	t.Helper()
	for index := range definition.Edges {
		edge := &definition.Edges[index]
		if edge.From == from && edge.To == to {
			return edge
		}
	}
	t.Fatalf("edge %q -> %q not found", from, to)
	return nil
}

func setConfig(t *testing.T, node *Node, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	node.Config = raw
}

func TestBuiltinFlowCompiles(t *testing.T) {
	compiled, err := LoadFile("../../testdata/flows/default-youtrack.json")
	if err != nil {
		t.Fatalf("compile builtin flow: %v", err)
	}
	if len(compiled.Nodes) != 11 {
		t.Fatalf("nodes = %d, want 11", len(compiled.Nodes))
	}
	if len(compiled.Order) != 11 {
		t.Fatalf("topological order = %d, want 11", len(compiled.Order))
	}
	edgeCount := 0
	for _, node := range compiled.Nodes {
		edgeCount += len(node.Next)
	}
	if edgeCount != 15 {
		t.Fatalf("edges = %d, want 15", edgeCount)
	}
	if compiled.Start != "start" {
		t.Fatalf("start = %q, want start", compiled.Start)
	}
	if len(compiled.Triggers) != 1 {
		t.Fatalf("triggers = %d, want 1", len(compiled.Triggers))
	}
	if compiled.Triggers[0].Trigger.ID != "all-youtrack-events" || compiled.Triggers[0].When != nil {
		t.Fatalf("unexpected trigger: %+v", compiled.Triggers[0].Trigger)
	}
	if compiled.Triggers[0].Trigger.Dedupe.Scope != "event" {
		t.Fatalf("dedupe scope = %q, want event", compiled.Triggers[0].Trigger.Dedupe.Scope)
	}

	counts := map[NodeType]int{}
	for _, node := range compiled.Nodes {
		counts[NodeType(node.Node.Type)]++
	}
	want := map[NodeType]int{
		NodeStart: 1, NodeEnsure: 1, NodeEvaluate: 1, NodeBranch: 1,
		NodeTurn: 4, NodeGrant: 1, NodeRevoke: 1, NodeEnd: 1,
	}
	if len(counts) != len(want) {
		t.Fatalf("node types = %v, want %v", counts, want)
	}
	for typ, count := range want {
		if counts[typ] != count {
			t.Fatalf("%s count = %d, want %d", typ, counts[typ], count)
		}
	}

	branch := compiled.Nodes["branch"]
	if len(branch.Next) != 6 {
		t.Fatalf("branch edges = %d, want 6", len(branch.Next))
	}
	if branch.Next[len(branch.Next)-1].When != nil {
		t.Fatal("the last branch edge must be the default")
	}
	for index, edge := range branch.Next[:len(branch.Next)-1] {
		if edge.When == nil {
			t.Fatalf("branch edge %d has no condition", index)
		}
	}

	analyze := compiled.Nodes["analyze"]
	if !analyze.Ancestors["evaluate"] {
		t.Fatal("evaluate must be an ancestor of analyze")
	}
	if analyze.Ancestors["execute"] {
		t.Fatal("execute must not be an ancestor of analyze")
	}
	turn, ok := analyze.Config.(*TurnConfig)
	if !ok || turn.Action != "analyze" || turn.Phase != "analysis" || !turn.RequireReply {
		t.Fatalf("unexpected analyze config: %#v", analyze.Config)
	}
	if _, ok := compiled.Nodes["evaluate"].Config.(*EvaluateConfig); !ok {
		t.Fatalf("evaluate config = %T", compiled.Nodes["evaluate"].Config)
	}
	if _, ok := compiled.Nodes["permit"].Config.(*GrantConfig); !ok {
		t.Fatalf("permit config = %T", compiled.Nodes["permit"].Config)
	}
	if compiled.Limits.Timeout != 0 || compiled.Limits.MaxCost != 0 {
		t.Fatalf("unexpected limits: %+v", compiled.Limits)
	}
}

func TestBuiltinTaskRefs(t *testing.T) {
	compiled, err := LoadFile("../../testdata/flows/default-youtrack.json")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	for id, node := range compiled.Nodes {
		ref := taskRefOf(node.Config)
		if ref == "" {
			continue
		}
		if ref != "primary" {
			t.Fatalf("node %q references task %q", id, ref)
		}
	}
	permit := compiled.Nodes["permit"].Config.(*GrantConfig)
	if permit.Mode != "refusals" || len(permit.List) != 0 {
		t.Fatalf("unexpected grant config: %+v", permit)
	}
}

func TestDecodeStrict(t *testing.T) {
	data, err := os.ReadFile("../../testdata/flows/default-youtrack.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	withUnknown := strings.Replace(string(data), `"version": 1,`, `"version": 1, "extra": true,`, 1)
	if _, err := Decode([]byte(withUnknown)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected an unknown field error, got %v", err)
	}
	if _, err := Decode(append(data, []byte("\n{}\n")...)); err == nil || !strings.Contains(err.Error(), "trailing data") {
		t.Fatalf("expected a trailing data error, got %v", err)
	}
}

func TestCompileRejects(t *testing.T) {
	rawTurn := func(action string) map[string]any {
		return map[string]any{"task": "primary", "action": action, "phase": "analysis"}
	}
	tests := []struct {
		name   string
		want   string
		mutate func(*testing.T, *Definition)
	}{
		{
			name: "wrong version",
			want: "version 2",
			mutate: func(t *testing.T, definition *Definition) {
				definition.Version = 2
			},
		},
		{
			name: "bad flow id",
			want: "id must match",
			mutate: func(t *testing.T, definition *Definition) {
				definition.ID = "Bad ID"
			},
		},
		{
			name: "missing name",
			want: "name is required",
			mutate: func(t *testing.T, definition *Definition) {
				definition.Name = " "
			},
		},
		{
			name: "missing upstream",
			want: "upstream is required",
			mutate: func(t *testing.T, definition *Definition) {
				definition.Upstream = ""
			},
		},
		{
			name: "duplicate trigger",
			want: "duplicate trigger id",
			mutate: func(t *testing.T, definition *Definition) {
				definition.Triggers = append(definition.Triggers, definition.Triggers[0])
			},
		},
		{
			name: "bad dedupe scope",
			want: "dedupe scope",
			mutate: func(t *testing.T, definition *Definition) {
				definition.Triggers[0].Dedupe.Scope = "forever"
			},
		},
		{
			name: "trigger reads task state",
			want: "unknown path",
			mutate: func(t *testing.T, definition *Definition) {
				definition.Triggers[0].When = "tasks.primary.state == 'awaiting_input'"
			},
		},
		{
			name: "trigger condition is not bool",
			want: "condition must be bool",
			mutate: func(t *testing.T, definition *Definition) {
				definition.Triggers[0].When = "trigger.subject.key"
			},
		},
		{
			name: "unsupported task identity",
			want: "not supported",
			mutate: func(t *testing.T, definition *Definition) {
				spec := definition.Tasks["primary"]
				spec.Identity = "flow"
				definition.Tasks["primary"] = spec
			},
		},
		{
			name: "duplicate node id",
			want: "duplicate node id",
			mutate: func(t *testing.T, definition *Definition) {
				definition.Nodes = append(definition.Nodes, definition.Nodes[4])
			},
		},
		{
			name: "unknown node type",
			want: "unknown node type",
			mutate: func(t *testing.T, definition *Definition) {
				nodeByID(t, definition, "analyze").Type = "deploy"
			},
		},
		{
			name: "reserved node type",
			want: "reserved",
			mutate: func(t *testing.T, definition *Definition) {
				nodeByID(t, definition, "analyze").Type = "http.request"
			},
		},
		{
			name: "unknown config field",
			want: "unknown field",
			mutate: func(t *testing.T, definition *Definition) {
				setConfig(t, nodeByID(t, definition, "analyze"), map[string]any{
					"task": "primary", "action": "analyze", "phase": "analysis", "bogus": true,
				})
			},
		},
		{
			name: "missing task ref",
			want: "is not declared",
			mutate: func(t *testing.T, definition *Definition) {
				setConfig(t, nodeByID(t, definition, "analyze"), map[string]any{
					"task": "secondary", "action": "analyze", "phase": "analysis",
				})
			},
		},
		{
			name: "bad action",
			want: "action",
			mutate: func(t *testing.T, definition *Definition) {
				setConfig(t, nodeByID(t, definition, "analyze"), rawTurn("deploy"))
			},
		},
		{
			name: "bad phase",
			want: "phase",
			mutate: func(t *testing.T, definition *Definition) {
				setConfig(t, nodeByID(t, definition, "analyze"), map[string]any{
					"task": "primary", "action": "analyze", "phase": "write",
				})
			},
		},
		{
			name: "bad end state",
			want: "state",
			mutate: func(t *testing.T, definition *Definition) {
				setConfig(t, nodeByID(t, definition, "end"), map[string]any{"state": "maybe"})
			},
		},
		{
			name: "non-branch condition",
			want: "only a branch",
			mutate: func(t *testing.T, definition *Definition) {
				edgeBetween(t, definition, "ensure", "evaluate").When = "true"
			},
		},
		{
			name: "non-branch with two edges",
			want: "exactly one outgoing",
			mutate: func(t *testing.T, definition *Definition) {
				definition.Edges = append(definition.Edges, Edge{From: "ensure", To: "end"})
			},
		},
		{
			name: "two branch defaults",
			want: "default",
			mutate: func(t *testing.T, definition *Definition) {
				edgeBetween(t, definition, "branch", "analyze").When = ""
			},
		},
		{
			name: "default not last",
			want: "default",
			mutate: func(t *testing.T, definition *Definition) {
				var moved Edge
				for index, edge := range definition.Edges {
					if edge.From == "branch" && edge.To == "end" {
						moved = edge
						definition.Edges = append(definition.Edges[:index], definition.Edges[index+1:]...)
						break
					}
				}
				definition.Edges = append([]Edge{moved}, definition.Edges...)
			},
		},
		{
			name: "unknown expression path",
			want: "unknown path",
			mutate: func(t *testing.T, definition *Definition) {
				edgeBetween(t, definition, "branch", "analyze").When = "steps.nope.output.action == 'x'"
			},
		},
		{
			name: "step is not an ancestor",
			want: "unknown path",
			mutate: func(t *testing.T, definition *Definition) {
				edgeBetween(t, definition, "branch", "analyze").When = "steps.execute.output.action == 'x'"
			},
		},
		{
			name: "prompt unknown path",
			want: "prompt",
			mutate: func(t *testing.T, definition *Definition) {
				setConfig(t, nodeByID(t, definition, "analyze"), map[string]any{
					"task": "primary", "action": "analyze", "phase": "analysis",
					"prompt": "Look at {{ steps.nope.output.reply }}",
				})
			},
		},
		{
			name: "prompt renders an object",
			want: "renders",
			mutate: func(t *testing.T, definition *Definition) {
				setConfig(t, nodeByID(t, definition, "analyze"), map[string]any{
					"task": "primary", "action": "analyze", "phase": "analysis",
					"prompt": "Look at {{ trigger.subject }}",
				})
			},
		},
		{
			name: "bad limit",
			want: "limits.timeout",
			mutate: func(t *testing.T, definition *Definition) {
				definition.Limits = &Limits{Timeout: "soon"}
			},
		},
		{
			name: "negative timeout",
			want: "negative",
			mutate: func(t *testing.T, definition *Definition) {
				definition.Limits = &Limits{Timeout: "-1s"}
			},
		},
		{
			name: "negative cost",
			want: "max_cost",
			mutate: func(t *testing.T, definition *Definition) {
				definition.Limits = &Limits{MaxCost: -1}
			},
		},
		{
			name: "missing start",
			want: "start node is required",
			mutate: func(t *testing.T, definition *Definition) {
				removeNode(definition, "start")
			},
		},
		{
			name: "duplicate start",
			want: "more than one start",
			mutate: func(t *testing.T, definition *Definition) {
				definition.Nodes = append(definition.Nodes, Node{ID: "start2", Type: "start"})
			},
		},
		{
			name: "missing end",
			want: "end node is required",
			mutate: func(t *testing.T, definition *Definition) {
				removeNode(definition, "end")
			},
		},
		{
			name: "unreachable node",
			want: "unreachable",
			mutate: func(t *testing.T, definition *Definition) {
				definition.Nodes = append(definition.Nodes, Node{
					ID: "orphan", Type: "task.turn", Config: mustJSON(map[string]any{
						"task": "primary", "action": "analyze", "phase": "analysis",
					}),
				})
				definition.Edges = append(definition.Edges, Edge{From: "orphan", To: "end"})
			},
		},
		{
			name: "cycle",
			want: "cycle",
			mutate: func(t *testing.T, definition *Definition) {
				for _, id := range []string{"cycle-a", "cycle-b"} {
					definition.Nodes = append(definition.Nodes, Node{
						ID: id, Type: "task.turn", Config: mustJSON(map[string]any{
							"task": "primary", "action": "analyze", "phase": "analysis",
						}),
					})
				}
				definition.Edges = append(definition.Edges,
					Edge{From: "cycle-a", To: "cycle-b"},
					Edge{From: "cycle-b", To: "cycle-a"},
				)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			definition := cloneDefinition(t, fixtureDefinition(t))
			test.mutate(t, &definition)
			_, err := Compile(definition)
			if err == nil {
				t.Fatalf("expected an error containing %q", test.want)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %q does not contain %q", err, test.want)
			}
		})
	}
}

func mustJSON(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return raw
}

func removeNode(definition *Definition, id string) {
	nodes := definition.Nodes[:0]
	for _, node := range definition.Nodes {
		if node.ID != id {
			nodes = append(nodes, node)
		}
	}
	definition.Nodes = nodes
	edges := definition.Edges[:0]
	for _, edge := range definition.Edges {
		if edge.From != id && edge.To != id {
			edges = append(edges, edge)
		}
	}
	definition.Edges = edges
}

func TestGrantCommandsList(t *testing.T) {
	definition := fixtureDefinition(t)
	setConfig(t, nodeByID(t, &definition, "permit"), map[string]any{
		"task":     "primary",
		"commands": []string{"git push", "rm -rf build"},
	})
	compiled, err := Compile(definition)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	grant := compiled.Nodes["permit"].Config.(*GrantConfig)
	if grant.Mode != "" || len(grant.List) != 2 || grant.List[0] != "git push" {
		t.Fatalf("unexpected grant config: %+v", grant)
	}
}
