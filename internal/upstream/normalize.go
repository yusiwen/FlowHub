package upstream

import (
	"time"

	"github.com/yusiwen/flowhub/internal/source"
)

// Normalizer is what a source adapter implements in addition to source.Source:
// it turns the decoded delivery into the neutral envelope. The receiver checks the
// interface only when an event log is configured, so an adapter that predates the
// flow model keeps working.
type Normalizer interface {
	Normalize(decoded source.Decoded, request *source.Request, received time.Time) (Event, error)
}
