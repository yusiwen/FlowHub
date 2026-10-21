package flow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/yusiwen/flowhub/internal/flow/expr"
)

// NodeType is the type of a graph vertex. Only the eight v1 types have an
// executor; the reserved names are recognised so they can be refused with a
// specific message instead of "unknown type".
type NodeType string

const (
	NodeStart    NodeType = "start"
	NodeEnsure   NodeType = "task.ensure"
	NodeEvaluate NodeType = "task.evaluate"
	NodeTurn     NodeType = "task.turn"
	NodeGrant    NodeType = "task.grant"
	NodeRevoke   NodeType = "task.revoke"
	NodeBranch   NodeType = "branch"
	NodeEnd      NodeType = "end"

	NodeHTTP     NodeType = "http.request"
	NodeApproval NodeType = "human.approval"
	NodeTimer    NodeType = "wait.timer"
	NodeNotify   NodeType = "notify.tracker"
	NodeParallel NodeType = "parallel"
	NodeJoin     NodeType = "join"
	NodeSubflow  NodeType = "subflow"
)

var reservedNodeTypes = map[NodeType]bool{
	NodeHTTP:     true,
	NodeApproval: true,
	NodeTimer:    true,
	NodeNotify:   true,
	NodeParallel: true,
	NodeJoin:     true,
	NodeSubflow:  true,
}

// NodeConfig is the typed configuration of one node.
type NodeConfig interface {
	nodeConfig()
}

// StartConfig is empty; the node exists to give the graph one entry.
type StartConfig struct{}

func (*StartConfig) nodeConfig() {}

// EnsureConfig resolves the task and the workspace.
type EnsureConfig struct {
	Task string `json:"task"`
}

func (*EnsureConfig) nodeConfig() {}

// EvaluateConfig runs the upstream profile's decision for one task.
type EvaluateConfig struct {
	Task   string `json:"task"`
	Policy string `json:"policy"`
}

func (*EvaluateConfig) nodeConfig() {}

// TurnConfig runs one agent turn. Prompt is optional; the builtin flow takes it
// from the upstream profile, while a custom flow may add a template.
type TurnConfig struct {
	Task         string `json:"task"`
	Action       string `json:"action"`
	Phase        string `json:"phase"`
	RequireReply bool   `json:"require_reply,omitempty"`
	Prompt       string `json:"prompt,omitempty"`

	Template *expr.Template `json:"-"`
}

func (*TurnConfig) nodeConfig() {}

// GrantConfig grants the refusals of the most recent turn, or an explicit set of
// commands that must be a subset of them. Commands is either the string
// "refusals" or a non-empty array of strings.
type GrantConfig struct {
	Task     string          `json:"task"`
	Commands json.RawMessage `json:"commands"`

	Mode string   `json:"-"`
	List []string `json:"-"`
}

func (*GrantConfig) nodeConfig() {}

// RevokeConfig clears the task's grant.
type RevokeConfig struct {
	Task string `json:"task"`
}

func (*RevokeConfig) nodeConfig() {}

// BranchConfig is empty; the conditions live on the outgoing edges.
type BranchConfig struct{}

func (*BranchConfig) nodeConfig() {}

// EndConfig terminates the run.
type EndConfig struct {
	State string `json:"state,omitempty"`
}

func (*EndConfig) nodeConfig() {}

func strictConfig(raw json.RawMessage, target any) error {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		raw = []byte("{}")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("trailing data in node config")
	}
	return nil
}

func decodeNodeConfig(typ string, raw json.RawMessage) (NodeConfig, error) {
	nodeType := NodeType(typ)
	if reservedNodeTypes[nodeType] {
		return nil, fmt.Errorf("node type %q is reserved and not implemented in this version", typ)
	}
	switch nodeType {
	case NodeStart:
		config := &StartConfig{}
		if err := strictConfig(raw, config); err != nil {
			return nil, err
		}
		return config, nil
	case NodeEnsure:
		config := &EnsureConfig{}
		if err := strictConfig(raw, config); err != nil {
			return nil, err
		}
		if strings.TrimSpace(config.Task) == "" {
			return nil, fmt.Errorf("task is required")
		}
		return config, nil
	case NodeEvaluate:
		config := &EvaluateConfig{}
		if err := strictConfig(raw, config); err != nil {
			return nil, err
		}
		if strings.TrimSpace(config.Task) == "" {
			return nil, fmt.Errorf("task is required")
		}
		if config.Policy != "upstream" {
			return nil, fmt.Errorf("policy must be %q", "upstream")
		}
		return config, nil
	case NodeTurn:
		config := &TurnConfig{}
		if err := strictConfig(raw, config); err != nil {
			return nil, err
		}
		if strings.TrimSpace(config.Task) == "" {
			return nil, fmt.Errorf("task is required")
		}
		switch config.Action {
		case "analyze", "plan", "execute", "last":
		default:
			return nil, fmt.Errorf("action %q is not one of analyze, plan, execute, last", config.Action)
		}
		switch config.Phase {
		case "analysis", "execution", "auto":
		default:
			return nil, fmt.Errorf("phase %q is not one of analysis, execution, auto", config.Phase)
		}
		return config, nil
	case NodeGrant:
		config := &GrantConfig{}
		if err := strictConfig(raw, config); err != nil {
			return nil, err
		}
		if strings.TrimSpace(config.Task) == "" {
			return nil, fmt.Errorf("task is required")
		}
		if err := parseGrantCommands(config); err != nil {
			return nil, err
		}
		return config, nil
	case NodeRevoke:
		config := &RevokeConfig{}
		if err := strictConfig(raw, config); err != nil {
			return nil, err
		}
		if strings.TrimSpace(config.Task) == "" {
			return nil, fmt.Errorf("task is required")
		}
		return config, nil
	case NodeBranch:
		config := &BranchConfig{}
		if err := strictConfig(raw, config); err != nil {
			return nil, err
		}
		return config, nil
	case NodeEnd:
		config := &EndConfig{}
		if err := strictConfig(raw, config); err != nil {
			return nil, err
		}
		if config.State == "" {
			config.State = "succeeded"
		}
		switch config.State {
		case "succeeded", "failed":
		default:
			return nil, fmt.Errorf("state %q is not one of succeeded, failed", config.State)
		}
		return config, nil
	default:
		return nil, fmt.Errorf("unknown node type %q", typ)
	}
}

func parseGrantCommands(config *GrantConfig) error {
	if len(bytes.TrimSpace(config.Commands)) == 0 {
		return fmt.Errorf("commands is required")
	}
	var mode string
	if err := json.Unmarshal(config.Commands, &mode); err == nil {
		if mode != "refusals" {
			return fmt.Errorf(`commands must be "refusals" or a non-empty array of commands`)
		}
		config.Mode = mode
		return nil
	}
	var commands []string
	if err := json.Unmarshal(config.Commands, &commands); err != nil {
		return fmt.Errorf(`commands must be "refusals" or a non-empty array of commands`)
	}
	if len(commands) == 0 {
		return fmt.Errorf("commands must not be empty")
	}
	for _, command := range commands {
		if strings.TrimSpace(command) == "" {
			return fmt.Errorf("commands must not contain an empty command")
		}
	}
	config.List = commands
	return nil
}

func taskRefOf(config NodeConfig) string {
	switch typed := config.(type) {
	case *EnsureConfig:
		return typed.Task
	case *EvaluateConfig:
		return typed.Task
	case *TurnConfig:
		return typed.Task
	case *GrantConfig:
		return typed.Task
	case *RevokeConfig:
		return typed.Task
	default:
		return ""
	}
}
