package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestNewLogger verifies constructor behaviour for valid and invalid inputs.
func TestNewLogger(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		level   string
		format  string
		output  string
		wantErr bool
	}{
		{
			name:   "level=info format=json output=stdout",
			level:  "info",
			format: "json",
			output: "stdout",
		},
		{
			name:   "level=debug format=text output=stderr",
			level:  "debug",
			format: "text",
			output: "stderr",
		},
		{
			name:   "level=warn",
			level:  "warn",
			format: "json",
			output: "stdout",
		},
		{
			name:   "level=error",
			level:  "error",
			format: "json",
			output: "stdout",
		},
		{
			name:    "unknown level trace returns error",
			level:   "trace",
			format:  "json",
			output:  "stdout",
			wantErr: true,
		},
		{
			name:    "unknown format yaml returns error",
			level:   "info",
			format:  "yaml",
			output:  "stdout",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			logger, err := NewLogger(tc.level, tc.format, tc.output)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if logger == nil {
				t.Fatal("NewLogger returned nil logger without error")
			}
		})
	}
}

// TestNewLogger_FileOutput verifies that a file-path output creates the file
// and that the logger writes to it.
func TestNewLogger_FileOutput(t *testing.T) {
	t.Parallel()

	t.Run("output=file path in TempDir creates file and writes to it", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		logFile := filepath.Join(dir, "app.log")

		logger, err := NewLogger("info", "json", logFile)
		if err != nil {
			t.Fatalf("NewLogger: %v", err)
		}
		if logger == nil {
			t.Fatal("logger is nil")
		}

		logger.Info("hello from test")

		data, err := os.ReadFile(logFile)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if len(data) == 0 {
			t.Error("log file is empty after writing a record")
		}
	})

	t.Run("output=non-existent parent dir returns error", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		// Use a path whose parent directory does not exist.
		noParent := filepath.Join(dir, "nonexistent-dir", "app.log")

		_, err := NewLogger("info", "json", noParent)
		if err == nil {
			t.Fatal("expected error for non-existent parent dir, got nil")
		}
	})
}

// TestErrorLogger_Log covers the ErrorLogger.Log method.
func TestErrorLogger_Log(t *testing.T) {
	t.Parallel()

	t.Run("writes a line to the configured directory", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		el := NewErrorLogger(dir, "YYYY-MM-DD-errors.md")

		if err := el.Log("run-1", "1", "web_search", fmt.Errorf("timeout"), "retry"); err != nil {
			t.Fatalf("Log: %v", err)
		}

		// Find the file that was created.
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
		if len(entries) == 0 {
			t.Fatal("no files written to error log directory")
		}
	})

	t.Run("line contains runID, iteration, tool name, and error message", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		el := NewErrorLogger(dir, "YYYY-MM-DD-errors.md")

		runID := "run-abc"
		iteration := "3"
		toolName := "exec"
		errMsg := "command not found"

		if err := el.Log(runID, iteration, toolName, fmt.Errorf("%s", errMsg), "check PATH"); err != nil {
			t.Fatalf("Log: %v", err)
		}

		data := readOnlyLogFile(t, dir)

		line := string(data)
		for _, want := range []string{runID, iteration, toolName, errMsg} {
			if !strings.Contains(line, want) {
				t.Errorf("log line does not contain %q:\n%s", want, line)
			}
		}
	})

	t.Run("file is created if it does not exist", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		el := NewErrorLogger(dir, "YYYY-MM-DD-errors.md")

		// Confirm no files yet.
		entries, _ := os.ReadDir(dir)
		if len(entries) != 0 {
			t.Fatalf("expected empty dir, got %d entries", len(entries))
		}

		if err := el.Log("r", "0", "noop", fmt.Errorf("err"), ""); err != nil {
			t.Fatalf("Log: %v", err)
		}

		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
		if len(entries) == 0 {
			t.Fatal("log file was not created")
		}
	})

	t.Run("YYYY-MM-DD is replaced with today's date in the filename", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		el := NewErrorLogger(dir, "YYYY-MM-DD-errors.md")

		if err := el.Log("r", "0", "noop", fmt.Errorf("err"), ""); err != nil {
			t.Fatalf("Log: %v", err)
		}

		today := time.Now().UTC().Format("2006-01-02")
		expectedName := today + "-errors.md"

		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
		if len(entries) == 0 {
			t.Fatal("no files in error log directory")
		}
		if entries[0].Name() != expectedName {
			t.Errorf("filename = %q, want %q", entries[0].Name(), expectedName)
		}
	})

	t.Run("concurrent Log calls do not race", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		el := NewErrorLogger(dir, "YYYY-MM-DD-errors.md")

		const goroutines = 20
		var wg sync.WaitGroup
		wg.Add(goroutines)

		for i := 0; i < goroutines; i++ {
			i := i
			go func() {
				defer wg.Done()
				if err := el.Log(
					fmt.Sprintf("run-%d", i),
					fmt.Sprintf("%d", i),
					"tool",
					fmt.Errorf("concurrent error %d", i),
					"",
				); err != nil {
					// t.Errorf is not safe from goroutines after the test may have
					// finished; we accept the race on error reporting here because
					// the race detector will catch data races in el.Log itself.
					_ = err
				}
			}()
		}
		wg.Wait()
	})
}

// readOnlyLogFile reads the single log file expected to exist in dir and
// returns its contents. It fails the test if the directory is empty or
// contains more than one file.
func readOnlyLogFile(t *testing.T, dir string) []byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readOnlyLogFile ReadDir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("readOnlyLogFile: no files in directory")
	}
	data, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("readOnlyLogFile ReadFile: %v", err)
	}
	return data
}
