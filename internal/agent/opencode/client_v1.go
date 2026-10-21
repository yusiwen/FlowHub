package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
)

// This file is the opencode 1.x transport. Every endpoint here was removed in
// opencode 2; the v2 equivalents are in client_v2.go. It stays because a FlowHub
// control plane may still route work to a host that has not been upgraded, and a
// runtime that silently stopped working on an upgrade is worse than one that
// speaks both generations (ADR 0012).

func (c *Client) healthV1(ctx context.Context) (Health, error) {
	var health Health
	err := c.do(ctx, http.MethodGet, "/global/health", nil, nil, &health)
	return health, err
}

func (c *Client) agentsV1(ctx context.Context) ([]AgentInfo, error) {
	var agents []AgentInfo
	err := c.do(ctx, http.MethodGet, "/agent", nil, nil, &agents)
	return agents, err
}

// catalogueV1 reads GET /provider, whose model map is keyed by model id.
func (c *Client) catalogueV1(ctx context.Context) (map[string][]string, error) {
	var payload struct {
		All []struct {
			ID     string                     `json:"id"`
			Models map[string]json.RawMessage `json:"models"`
		} `json:"all"`
	}
	if err := c.do(ctx, http.MethodGet, "/provider", nil, nil, &payload); err != nil {
		return nil, err
	}
	catalogue := make(map[string][]string, len(payload.All))
	for _, provider := range payload.All {
		models := make([]string, 0, len(provider.Models))
		for id := range provider.Models {
			models = append(models, id)
		}
		sort.Strings(models)
		catalogue[provider.ID] = models
	}
	return catalogue, nil
}

func (c *Client) createSessionV1(ctx context.Context, directory string, req CreateSessionRequest) (Session, error) {
	if len(req.Permissions) > 0 {
		return Session{}, fmt.Errorf("opencode: a v1 server cannot take the v2 permissions array; the runtime built the wrong ruleset")
	}
	var session Session
	query := url.Values{"directory": {directory}}
	err := c.do(ctx, http.MethodPost, "/session", query, req, &session)
	return session, err
}

func (c *Client) promptAsyncV1(ctx context.Context, directory, sessionID string, req PromptRequest) error {
	query := url.Values{"directory": {directory}}
	return c.do(ctx, http.MethodPost, "/session/"+sessionID+"/prompt_async", query, req, nil)
}

func (c *Client) sessionV1(ctx context.Context, directory, sessionID string) (Session, error) {
	var session Session
	query := url.Values{"directory": {directory}}
	err := c.do(ctx, http.MethodGet, "/session/"+sessionID, query, nil, &session)
	return session, err
}

func (c *Client) sessionStatusV1(ctx context.Context, directory, sessionID string) (Status, error) {
	var statuses map[string]Status
	query := url.Values{"directory": {directory}}
	if err := c.do(ctx, http.MethodGet, "/session/status", query, nil, &statuses); err != nil {
		return Status{}, err
	}
	return statuses[sessionID], nil
}

func (c *Client) messagesV1(ctx context.Context, directory, sessionID string) ([]Message, error) {
	var messages []Message
	query := url.Values{"directory": {directory}}
	err := c.do(ctx, http.MethodGet, "/session/"+sessionID+"/message", query, nil, &messages)
	return messages, err
}

// permissionsV1 lists every pending permission request in directory and keeps the
// ones belonging to sessionID: the v1 endpoint has no session filter.
func (c *Client) permissionsV1(ctx context.Context, directory, sessionID string) ([]PermissionRequest, error) {
	var requests []PermissionRequest
	query := url.Values{"directory": {directory}}
	if err := c.do(ctx, http.MethodGet, "/permission", query, nil, &requests); err != nil {
		return nil, err
	}
	mine := make([]PermissionRequest, 0, len(requests))
	for _, request := range requests {
		if request.SessionID == sessionID {
			mine = append(mine, request)
		}
	}
	return mine, nil
}

func (c *Client) replyPermissionV1(ctx context.Context, directory, requestID string, reply Reply, message string) error {
	body := ReplyRequest{Reply: reply, Message: message}
	query := url.Values{"directory": {directory}}
	return c.do(ctx, http.MethodPost, "/permission/"+requestID+"/reply", query, body, nil)
}
