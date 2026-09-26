package youtrack

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// maxSchemaDepth bounds the recursion for pathological payloads.
const maxSchemaDepth = 12

// Schema walks a payload and returns the flat, sorted list of JSON paths with the
// type found at each path, for example:
//
//	changedFields: array
//	changedFields[].name: string
//	changedFields[].value.name: string
//	project.key: string
//
// Array elements are merged into one `[]` path, so the report describes the union
// of every element. This is the quickest way to compare the real payload against
// what the design documents claim it contains (for example whether `project.id`
// or `numberInProject` is present, or whether a user object carries `id`).
func Schema(body []byte) ([]string, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()

	var root any
	if err := decoder.Decode(&root); err != nil {
		return nil, err
	}

	paths := map[string]struct{}{}
	walkSchema("", root, paths, 0)

	out := make([]string, 0, len(paths))
	for path := range paths {
		out = append(out, path)
	}
	// Sort by path, then by type, so a container is listed before the fields
	// inside it (`project: object` before `project.key: string`) and the report
	// reads like a schema rather than a byte-sorted dump.
	sort.Slice(out, func(i, j int) bool {
		leftPath, leftType := splitEntry(out[i])
		rightPath, rightType := splitEntry(out[j])
		if leftPath != rightPath {
			return leftPath < rightPath
		}
		return leftType < rightType
	})
	return out, nil
}

func splitEntry(entry string) (path, typeName string) {
	path, typeName, _ = strings.Cut(entry, ": ")
	return path, typeName
}

func walkSchema(path string, value any, out map[string]struct{}, depth int) {
	if depth > maxSchemaDepth {
		return
	}

	switch typed := value.(type) {
	case map[string]any:
		if path != "" {
			out[path+": object"] = struct{}{}
		}
		for key, child := range typed {
			walkSchema(joinPath(path, key), child, out, depth+1)
		}
	case []any:
		if path != "" {
			out[path+": array"] = struct{}{}
		}
		if len(typed) == 0 {
			return
		}
		for _, child := range typed {
			walkSchema(path+"[]", child, out, depth+1)
		}
	default:
		out[path+": "+schemaType(value)] = struct{}{}
	}
}

func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func schemaType(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case string:
		return "string"
	case json.Number:
		return "number"
	default:
		return fmt.Sprintf("%T", value)
	}
}
