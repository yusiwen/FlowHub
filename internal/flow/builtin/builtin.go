// Package builtin embeds the read-only flow that reproduces the current YouTrack
// pipeline.
//
// The canonical fixture lives in testdata/flows so the design review and the tests
// read one file; the copy here exists because //go:embed cannot reach outside the
// package directory. builtin_test.go fails if the two drift.
package builtin

import (
	_ "embed"
	"fmt"

	"github.com/yusiwen/flowhub/internal/flow"
)

//go:embed default-youtrack.json
var definition []byte

// Load decodes and compiles the built-in definition. It returns the raw bytes as
// well, because the revision a run records is a hash of what was executed.
func Load() (*flow.Compiled, []byte, error) {
	decoded, err := flow.Decode(definition)
	if err != nil {
		return nil, nil, fmt.Errorf("builtin flow: %w", err)
	}
	compiled, err := flow.Compile(decoded)
	if err != nil {
		return nil, nil, fmt.Errorf("builtin flow: %w", err)
	}
	return compiled, append([]byte(nil), definition...), nil
}
