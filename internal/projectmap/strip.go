package projectmap

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// stripDocumentationKeys removes every object member whose name starts with "_".
//
// The routing table is hand-maintained and JSON has no comments, so a
// "_comment" member anywhere in the tree documents the file without breaking the
// strict decoder that rejects typos in real field names.
//
// Values are carried as json.RawMessage throughout, so no value is ever
// re-encoded; only object member order changes.
func stripDocumentationKeys(raw []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return raw, nil
	}

	switch trimmed[0] {
	case '{':
		var object map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &object); err != nil {
			return nil, err
		}
		cleaned := make(map[string]json.RawMessage, len(object))
		for key, value := range object {
			if strings.HasPrefix(key, "_") {
				continue
			}
			inner, err := stripDocumentationKeys(value)
			if err != nil {
				return nil, fmt.Errorf("field %q: %w", key, err)
			}
			cleaned[key] = inner
		}
		return json.Marshal(cleaned)
	case '[':
		var list []json.RawMessage
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return nil, err
		}
		for index := range list {
			inner, err := stripDocumentationKeys(list[index])
			if err != nil {
				return nil, fmt.Errorf("item %d: %w", index, err)
			}
			list[index] = inner
		}
		return json.Marshal(list)
	default:
		return trimmed, nil
	}
}
