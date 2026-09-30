// Package microvm defines the Lambda MicroVM operations the provider uses, so the
// provider can be tested without AWS.
package microvm

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// State is a MicroVM lifecycle state.
type State string

const (
	StatePending    State = "pending"
	StateRunning    State = "running"
	StateSuspended  State = "suspended"
	StateTerminated State = "terminated"
	StateUnknown    State = "unknown"
)

// Launch describes an environment to start.
type Launch struct {
	ImageARN         string
	MaximumDuration  time.Duration
	IngressConnector string
	EgressConnector  string
	// ClientToken makes a repeated launch return the same environment.
	ClientToken string
	// ExecutionRoleARN is the IAM role the environment runs as, if any.
	ExecutionRoleARN string
}

// Environment is a launched MicroVM.
type Environment struct {
	ID       string
	Endpoint string
	State    State
}

// Token is an endpoint credential for the X-aws-proxy-auth header.
type Token struct {
	Value     string
	ExpiresAt time.Time
}

// API is the Lambda MicroVM surface the provider depends on.
type API interface {
	Launch(ctx context.Context, spec Launch) (Environment, error)
	// Describe returns an error with code CodeNotFound when the environment does not exist.
	Describe(ctx context.Context, id string) (Environment, error)
	MintToken(ctx context.Context, id string, port int32, lifetime time.Duration) (Token, error)
	// Terminate succeeds for an environment that is already gone.
	Terminate(ctx context.Context, id string) error
	// PostHook posts body to path on the environment's hook port and returns the HTTP status.
	PostHook(ctx context.Context, id, endpoint string, port int32, path, body string) (int, error)
	// List returns every environment in the region that is not terminated.
	List(ctx context.Context) ([]Environment, error)
}

// CodeNotFound is the Error code for a missing environment or image.
const CodeNotFound = "not_found"

// Error is a platform failure with a stable code that can cross the plugin boundary.
// Code and Message never contain account identifiers, ARNs, or endpoints.
type Error struct {
	Code      string
	Message   string
	Retryable bool
	Err       error
}

func (e *Error) Error() string {
	if e.Err == nil {
		return e.Code + ": " + e.Message
	}
	return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// Errorf builds an Error.
func Errorf(code string, retryable bool, err error, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Retryable: retryable, Err: err}
}

// AsError extracts an *Error from err.
func AsError(err error) (*Error, bool) {
	var apiErr *Error
	ok := errors.As(err, &apiErr)
	return apiErr, ok
}

// IsNotFound reports whether err is a CodeNotFound Error.
func IsNotFound(err error) bool {
	apiErr, ok := AsError(err)
	return ok && apiErr.Code == CodeNotFound
}
