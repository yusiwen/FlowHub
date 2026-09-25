package opencode

import (
	"context"
	"encoding/json"
	"sort"
)

// Catalogue lists the models each configured provider offers, as the server sees
// them: providerID -> modelIDs.
//
// A capability check uses it to answer a question the binary check cannot: the
// agent profile pins a model, and if that model is not in the catalogue every turn
// fails with ProviderModelNotFoundError. That happened here — a provider quietly
// dropped a model id — and it cost a task to discover, which is exactly what an
// enrolment check is for.
func (c *Client) Catalogue(ctx context.Context) (map[string][]string, error) {
	var payload struct {
		All []struct {
			ID     string                     `json:"id"`
			Models map[string]json.RawMessage `json:"models"`
		} `json:"all"`
	}
	if err := c.do(ctx, "GET", "/provider", nil, nil, &payload); err != nil {
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
