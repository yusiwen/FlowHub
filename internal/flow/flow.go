// Package flow holds the builtin flow definition and the compiler that turns it
// into a validated graph.
//
// The definition is data: strict JSON with an explicit version, a trigger set, a
// task map, and a directed acyclic graph of typed nodes. Compile is the only way to
// obtain a graph the engine may execute; it refuses a definition that names an
// unknown path, a missing task, a node that cannot be reached, or a condition that
// is not a boolean. The model is documented in docs/flow-model.md.
package flow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/yusiwen/flowhub/internal/flow/expr"
)

// Definition is the on-disk shape of a flow. Unknown fields are refused.
type Definition struct {
	Version     int                 `json:"version"`
	ID          string              `json:"id"`
	Name        string              `json:"name"`
	Description string              `json:"description,omitempty"`
	Builtin     bool                `json:"builtin,omitempty"`
	Upstream    string              `json:"upstream"`
	Triggers    []Trigger           `json:"triggers"`
	Tasks       map[string]TaskSpec `json:"tasks,omitempty"`
	Nodes       []Node              `json:"nodes"`
	Edges       []Edge              `json:"edges"`
	Limits      *Limits             `json:"limits,omitempty"`
}

// Trigger is one subscription over events.
type Trigger struct {
	ID        string `json:"id"`
	Event     string `json:"event"`
	Exclusive bool   `json:"exclusive,omitempty"`
	Priority  int    `json:"priority,omitempty"`
	Dedupe    Dedupe `json:"dedupe,omitempty"`
	When      string `json:"when,omitempty"`
}

// Dedupe is a trigger's idempotency scope.
type Dedupe struct {
	Scope string `json:"scope,omitempty"`
}

// TaskSpec names one task the flow can operate on.
type TaskSpec struct {
	// Identity is "upstream" for the builtin flow: (upstream, subject.key). A
	// future "flow" identity gives a run its own task.
	Identity string `json:"identity"`
	// Reuse means look the identity up and create it only when absent.
	Reuse bool `json:"reuse,omitempty"`
}

// Node is one vertex of the graph. Config is decoded per type.
type Node struct {
	ID     string          `json:"id"`
	Type   string          `json:"type"`
	Name   string          `json:"name,omitempty"`
	Config json.RawMessage `json:"config,omitempty"`
}

// Edge is one directed transition. When is an expression; an edge without one is
// the default and must be last on a branch.
type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
	When string `json:"when,omitempty"`
}

// Limits bounds a run.
type Limits struct {
	Timeout string  `json:"timeout,omitempty"`
	MaxCost float64 `json:"max_cost,omitempty"`
}

// Constraints is Limits with the duration parsed.
type Constraints struct {
	Timeout time.Duration
	MaxCost float64
}

// Decode reads a definition with strict JSON. A file that says something the
// binary does not understand is refused instead of half-honoured.
func Decode(data []byte) (Definition, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var definition Definition
	if err := decoder.Decode(&definition); err != nil {
		return Definition{}, fmt.Errorf("decode flow: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Definition{}, fmt.Errorf("decode flow: trailing data after the definition")
	}
	return definition, nil
}

// LoadFile reads and compiles a definition. It is the convenience path for the
// builtin fixture and the later user-flow loader.
func LoadFile(path string) (*Compiled, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read flow %s: %w", path, err)
	}
	definition, err := Decode(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	compiled, err := Compile(definition)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return compiled, nil
}

// Compiled is a definition whose graph, configs and expressions have been
// validated. It is immutable after Compile returns.
type Compiled struct {
	Definition Definition
	Nodes      map[string]*CompiledNode
	Order      []string
	Start      string
	Limits     Constraints
	Triggers   []CompiledTrigger
}

// CompiledNode is one validated node and its outgoing and incoming edges.
type CompiledNode struct {
	Node      Node
	Config    NodeConfig
	Next      []*CompiledEdge
	In        []*CompiledEdge
	Ancestors map[string]bool
}

// CompiledEdge is one edge with its compiled condition (nil for a default edge).
type CompiledEdge struct {
	Edge Edge
	When *expr.Program
}

// CompiledTrigger is one trigger with its compiled condition (nil when absent).
type CompiledTrigger struct {
	Trigger Trigger
	When    *expr.Program
}
