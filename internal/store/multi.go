package store

import "errors"

// multi fans one record out to several recorders, so the same delivery lands in
// the machine readable audit log and in the human readable payload log.
type multi []Recorder

// NewMulti combines recorders into a single Recorder. Every recorder is always
// attempted, even if an earlier one fails.
func NewMulti(recorders ...Recorder) Recorder {
	filtered := make(multi, 0, len(recorders))
	for _, recorder := range recorders {
		if recorder != nil {
			filtered = append(filtered, recorder)
		}
	}
	return filtered
}

func (m multi) Record(rec *Record) error {
	var errs []error
	for _, recorder := range m {
		if err := recorder.Record(rec); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m multi) Close() error {
	var errs []error
	for _, recorder := range m {
		if err := recorder.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
