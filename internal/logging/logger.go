// Package logging provides structured logging utilities for the gpt-oss-executor.
// It wraps the standard library log/slog package and adds an ErrorLogger that
// appends human-readable error records to a daily markdown file.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// NewLogger constructs a *slog.Logger configured by three string parameters so
// that callers can drive it from config files or environment variables without
// importing slog themselves.
//
// level  — "debug", "info", "warn", or "error" (case-insensitive).
// format — "json" (default) or "text".
// output — "stdout" (default), "stderr", or an absolute/relative file path.
//
// When output is a file path the file is opened in append+create mode with
// 0644 permissions. The caller is responsible for closing the underlying file
// when the process exits; for file outputs this is best done via os.Exit
// defer chains rather than here, because *slog.Logger does not expose its
// writer.
func NewLogger(level, format, output string) (*slog.Logger, error) {
	// -- resolve log level --------------------------------------------------
	var slogLevel slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		slogLevel = slog.LevelDebug
	case "warn", "warning":
		slogLevel = slog.LevelWarn
	case "error":
		slogLevel = slog.LevelError
	case "info", "":
		slogLevel = slog.LevelInfo
	default:
		return nil, fmt.Errorf("logging: unknown level %q: must be one of debug, info, warn, error", level)
	}

	// -- resolve output writer ----------------------------------------------
	var w io.Writer
	switch strings.ToLower(strings.TrimSpace(output)) {
	case "stdout", "":
		w = os.Stdout
	case "stderr":
		w = os.Stderr
	default:
		// Treat as a file path. Open in append mode so restarts accumulate logs.
		f, err := os.OpenFile(output, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, fmt.Errorf("logging: opening log file %q: %w", output, err)
		}
		w = f
	}

	// -- build handler ------------------------------------------------------
	opts := &slog.HandlerOptions{Level: slogLevel}

	var handler slog.Handler
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "text":
		handler = slog.NewTextHandler(w, opts)
	case "json", "":
		handler = slog.NewJSONHandler(w, opts)
	default:
		return nil, fmt.Errorf("logging: unknown format %q: must be json or text", format)
	}

	return slog.New(handler), nil
}

// ErrorLogger appends structured error records to a daily markdown file.
// The filename template must contain the literal substring "YYYY-MM-DD" which
// is replaced at write time with the current UTC date, creating one file per
// calendar day.
//
// All public methods are safe for concurrent use.
type ErrorLogger struct {
	// Dir is the directory that will contain the daily log files. It is
	// created (with MkdirAll) on first use if it does not already exist.
	Dir string

	// Filename is the file name template, e.g. "YYYY-MM-DD-errors.md".
	// The substring "YYYY-MM-DD" is replaced with the current UTC date.
	Filename string

	mu sync.Mutex
}

// NewErrorLogger constructs an ErrorLogger. No filesystem I/O is performed
// until Log is called.
//
// dir      — directory in which daily log files are written.
// filename — file name template containing "YYYY-MM-DD" as a placeholder,
//
//	e.g. "YYYY-MM-DD-errors.md".
func NewErrorLogger(dir, filename string) *ErrorLogger {
	return &ErrorLogger{
		Dir:      dir,
		Filename: filename,
	}
}

// Log appends one error record to today's markdown file. The record format is:
//
//	[HH:MM:SS] RunID: <runID> | Iter: <iteration> | Tool: <toolName> | Error: <err> | Fix: <fix>
//
// The method creates the directory and file if they do not exist.
// It is safe to call Log from multiple goroutines simultaneously.
func (el *ErrorLogger) Log(runID, iteration, toolName string, err error, fix string) error {
	now := time.Now().UTC()

	date := now.Format("2006-01-02")      // YYYY-MM-DD
	timeStr := now.Format("15:04:05")     // HH:MM:SS

	filename := strings.ReplaceAll(el.Filename, "YYYY-MM-DD", date)
	path := filepath.Join(el.Dir, filename)

	line := fmt.Sprintf(
		"[%s] RunID: %s | Iter: %s | Tool: %s | Error: %v | Fix: %s\n",
		timeStr, runID, iteration, toolName, err, fix,
	)

	el.mu.Lock()
	defer el.mu.Unlock()

	if mkErr := os.MkdirAll(el.Dir, 0o755); mkErr != nil {
		return fmt.Errorf("logging: creating error log directory %q: %w", el.Dir, mkErr)
	}

	f, openErr := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if openErr != nil {
		return fmt.Errorf("logging: opening error log file %q: %w", path, openErr)
	}
	defer f.Close()

	if _, writeErr := fmt.Fprint(f, line); writeErr != nil {
		return fmt.Errorf("logging: writing to error log file %q: %w", path, writeErr)
	}

	return nil
}
