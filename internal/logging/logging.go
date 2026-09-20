package logging

import (
	"io"
	"log/slog"
	"os"
)

// Config describes the application logger.
type Config struct {
	// Level is one of debug, info, warn, error.
	Level string
	// Format is "text" or "json".
	Format string
	// File is the log file path. Empty, "-" or "none" disables file logging and
	// leaves stderr as the only sink.
	File string
	// MaxBytes is the rotation threshold for File.
	MaxBytes int64
	// MaxFiles is how many rotated archives to keep.
	MaxFiles int
}

// New builds the logger and returns a closer for the log file (nil when file
// logging is disabled).
func New(cfg Config) (*slog.Logger, io.Closer, error) {
	options := &slog.HandlerOptions{Level: parseLevel(cfg.Level)}

	var (
		sink   io.Writer = os.Stderr
		closer io.Closer
	)
	if fileEnabled(cfg.File) {
		file, err := OpenRotatingFile(cfg.File, cfg.MaxBytes, cfg.MaxFiles)
		if err != nil {
			return nil, nil, err
		}
		sink = io.MultiWriter(os.Stderr, file)
		closer = file
	}

	return slog.New(newHandler(cfg.Format, sink, options)), closer, nil
}

// FileEnabled reports whether cfg would write to a file.
func FileEnabled(path string) bool { return fileEnabled(path) }

func fileEnabled(path string) bool {
	return path != "" && path != "-" && path != "none"
}

func newHandler(format string, sink io.Writer, options *slog.HandlerOptions) slog.Handler {
	if format == "json" {
		return slog.NewJSONHandler(sink, options)
	}
	return slog.NewTextHandler(sink, options)
}

func parseLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
