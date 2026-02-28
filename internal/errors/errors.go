// Package errors defines custom error types and sentinel errors for the
// gpt-oss-executor. All errors carry a machine-readable Code that callers
// can inspect without string matching, and optionally wrap an underlying
// cause so that errors.Is / errors.As chains work correctly.
package errors

import (
	"context"
	"errors"
	"fmt"
)

// ExecutorError is the single concrete error type used throughout the executor.
// Code is a stable, machine-readable identifier; Message is a human-readable
// description. Cause, when non-nil, is the underlying error that triggered
// this one.
type ExecutorError struct {
	Code    string
	Message string
	Cause   error
}

// Error implements the error interface.
func (e *ExecutorError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("[%s] %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("[%s] %s", e.Code, e.Message)
}

// Unwrap returns the underlying cause so that errors.Is and errors.As can
// traverse the chain.
func (e *ExecutorError) Unwrap() error {
	return e.Cause
}

// Sentinel errors. These are package-level values that callers compare with
// errors.Is. Because ExecutorError is a struct, each sentinel is a pointer;
// errors.Is matches by value equality on the Code field via Is() below.

// ErrGptOssUnreachable is returned when the upstream GPT-OSS API cannot be
// reached (network failure, DNS error, etc.).
var ErrGptOssUnreachable = &ExecutorError{
	Code:    "gpt_oss_unreachable",
	Message: "GPT-OSS endpoint is unreachable",
}

// ErrMaxIterations is returned when the executor exhausts its iteration budget
// before the run completes.
var ErrMaxIterations = &ExecutorError{
	Code:    "max_iterations_exceeded",
	Message: "maximum iteration count exceeded",
}

// ErrRunTimeout is returned when the overall run deadline is exceeded.
var ErrRunTimeout = &ExecutorError{
	Code:    "timeout_exceeded",
	Message: "run timeout exceeded",
}

// ErrContextWindow is returned when the accumulated prompt would exceed the
// model's context window limit.
var ErrContextWindow = &ExecutorError{
	Code:    "context_window_exceeded",
	Message: "model context window exceeded",
}

// ErrToolNotFound is returned when a tool referenced by the model has not
// been registered with the executor.
var ErrToolNotFound = &ExecutorError{
	Code:    "tool_not_found",
	Message: "requested tool is not registered",
}

// ErrToolExecution is returned when a registered tool returns an error during
// execution.
var ErrToolExecution = &ExecutorError{
	Code:    "tool_execution_failed",
	Message: "tool execution failed",
}

// ErrEmptyReasoning is returned when the model produces a response with no
// reasoning content.
var ErrEmptyReasoning = &ExecutorError{
	Code:    "empty_reasoning",
	Message: "model returned empty reasoning",
}

// ErrNoToolIntents is returned when the model response contains no tool
// invocation intents where at least one was expected.
var ErrNoToolIntents = &ExecutorError{
	Code:    "no_tool_intents",
	Message: "model response contained no tool intents",
}

// Is makes errors.Is work correctly for ExecutorError sentinels. Two
// ExecutorErrors are considered equal when their Code fields match,
// regardless of Message or Cause. This allows callers to wrap a sentinel
// with additional context (via Wrap) while still matching with errors.Is.
func (e *ExecutorError) Is(target error) bool {
	var t *ExecutorError
	if errors.As(target, &t) {
		return e.Code == t.Code
	}
	return false
}

// Wrap returns a new ExecutorError that shares the code and message of base
// but records cause as its underlying error. Use this when you want to attach
// a root cause to a sentinel:
//
//	return errors.Wrap(errors.ErrToolExecution, err)
func Wrap(base *ExecutorError, cause error) *ExecutorError {
	return &ExecutorError{
		Code:    base.Code,
		Message: base.Message,
		Cause:   cause,
	}
}

// IsContextWindowError reports whether err, or any error in its chain, has the
// code "context_window_exceeded".
func IsContextWindowError(err error) bool {
	return errors.Is(err, ErrContextWindow)
}

// IsTransientError reports whether the error is one that a caller may
// reasonably retry. Transient errors are:
//   - gpt_oss_unreachable
//   - tool_execution_failed
//
// Non-transient errors include context_window_exceeded, max_iterations_exceeded,
// timeout_exceeded, and the standard library context errors
// (context.Canceled, context.DeadlineExceeded).
func IsTransientError(err error) bool {
	// Standard library context errors are terminal for the current run.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var execErr *ExecutorError
	if !errors.As(err, &execErr) {
		// Unknown error type — treat as non-transient to avoid blind retries.
		return false
	}

	switch execErr.Code {
	case ErrGptOssUnreachable.Code, ErrToolExecution.Code:
		return true
	default:
		return false
	}
}
