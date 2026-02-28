package errors

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// TestExecutorError_Error verifies the Error() string format.
func TestExecutorError_Error(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  *ExecutorError
		want string
	}{
		{
			name: "without cause: format is [code] message",
			err: &ExecutorError{
				Code:    "some_code",
				Message: "something went wrong",
			},
			want: "[some_code] something went wrong",
		},
		{
			name: "with cause: format is [code] message: cause text",
			err: &ExecutorError{
				Code:    "some_code",
				Message: "something went wrong",
				Cause:   fmt.Errorf("root cause"),
			},
			want: "[some_code] something went wrong: root cause",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWrap exercises the Wrap helper.
func TestWrap(t *testing.T) {
	t.Parallel()

	sentinel := ErrToolExecution
	cause := fmt.Errorf("exec failed: exit code 1")

	t.Run("wrapped error has same Code as sentinel", func(t *testing.T) {
		t.Parallel()
		wrapped := Wrap(sentinel, cause)
		if wrapped.Code != sentinel.Code {
			t.Errorf("Code = %q, want %q", wrapped.Code, sentinel.Code)
		}
	})

	t.Run("Wrap does not mutate the sentinel", func(t *testing.T) {
		t.Parallel()
		_ = Wrap(sentinel, cause)
		if sentinel.Cause != nil {
			t.Errorf("sentinel.Cause was mutated: got %v, want nil", sentinel.Cause)
		}
	})

	t.Run("errors.Is(wrapped, sentinel) returns true", func(t *testing.T) {
		t.Parallel()
		wrapped := Wrap(sentinel, cause)
		if !errors.Is(wrapped, sentinel) {
			t.Errorf("errors.Is(wrapped, sentinel) = false, want true")
		}
	})

	t.Run("errors.Unwrap(wrapped) returns the cause", func(t *testing.T) {
		t.Parallel()
		wrapped := Wrap(sentinel, cause)
		if got := errors.Unwrap(wrapped); got != cause {
			t.Errorf("errors.Unwrap = %v, want %v", got, cause)
		}
	})
}

// TestExecutorError_Is verifies the Is method used by errors.Is.
func TestExecutorError_Is(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		err    *ExecutorError
		target error
		want   bool
	}{
		{
			name: "same code matches different instances",
			err:  &ExecutorError{Code: "tool_execution_failed", Message: "msg a"},
			target: &ExecutorError{Code: "tool_execution_failed", Message: "msg b"},
			want: true,
		},
		{
			name: "different code does not match",
			err:  &ExecutorError{Code: "code_a", Message: "msg"},
			target: &ExecutorError{Code: "code_b", Message: "msg"},
			want: false,
		},
		{
			name:   "non-ExecutorError target returns false",
			err:    &ExecutorError{Code: "code_a", Message: "msg"},
			target: fmt.Errorf("plain error"),
			want:   false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.err.Is(tc.target); got != tc.want {
				t.Errorf("Is() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestIsTransientError covers the full set of inputs described in the spec.
func TestIsTransientError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "ErrGptOssUnreachable is transient",
			err:  ErrGptOssUnreachable,
			want: true,
		},
		{
			name: "ErrToolExecution is transient",
			err:  ErrToolExecution,
			want: true,
		},
		{
			name: "ErrContextWindow is not transient",
			err:  ErrContextWindow,
			want: false,
		},
		{
			name: "ErrMaxIterations is not transient",
			err:  ErrMaxIterations,
			want: false,
		},
		{
			name: "ErrRunTimeout is not transient",
			err:  ErrRunTimeout,
			want: false,
		},
		{
			name: "context.Canceled is not transient",
			err:  context.Canceled,
			want: false,
		},
		{
			name: "context.DeadlineExceeded is not transient",
			err:  context.DeadlineExceeded,
			want: false,
		},
		{
			name: "plain fmt.Errorf is not transient",
			err:  fmt.Errorf("something unexpected"),
			want: false,
		},
		{
			name: "Wrap(ErrGptOssUnreachable, cause) is transient",
			err:  Wrap(ErrGptOssUnreachable, fmt.Errorf("dial failed")),
			want: true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsTransientError(tc.err); got != tc.want {
				t.Errorf("IsTransientError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestIsContextWindowError covers the IsContextWindowError helper.
func TestIsContextWindowError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "ErrContextWindow returns true",
			err:  ErrContextWindow,
			want: true,
		},
		{
			name: "Wrapped ErrContextWindow returns true",
			err:  Wrap(ErrContextWindow, fmt.Errorf("context overflow")),
			want: true,
		},
		{
			name: "ErrToolExecution returns false",
			err:  ErrToolExecution,
			want: false,
		},
		{
			name: "plain error returns false",
			err:  fmt.Errorf("unrelated"),
			want: false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsContextWindowError(tc.err); got != tc.want {
				t.Errorf("IsContextWindowError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
