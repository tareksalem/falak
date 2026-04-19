package core

import (
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Sentinel errors for the core API. Transport layers map these to
// protocol-specific error codes (gRPC status, HTTP status, CLI exit code).
var (
	ErrNotFound         = errors.New("resource not found")
	ErrAlreadyExists    = errors.New("resource already exists")
	ErrInvalidArgument  = errors.New("invalid argument")
	ErrUnauthenticated  = errors.New("unauthenticated")
	ErrPermissionDenied = errors.New("permission denied")
	ErrUnavailable      = errors.New("service unavailable")
	ErrInternal         = errors.New("internal error")
)

// ToGRPCError converts a core API error to a gRPC status error with
// the appropriate code and the original error message preserved. The
// message is always included so operators get actionable diagnostics.
func ToGRPCError(err error) error {
	if err == nil {
		return nil
	}

	code := codes.Internal
	switch {
	case errors.Is(err, ErrNotFound):
		code = codes.NotFound
	case errors.Is(err, ErrAlreadyExists):
		code = codes.AlreadyExists
	case errors.Is(err, ErrInvalidArgument):
		code = codes.InvalidArgument
	case errors.Is(err, ErrUnauthenticated):
		code = codes.Unauthenticated
	case errors.Is(err, ErrPermissionDenied):
		code = codes.PermissionDenied
	case errors.Is(err, ErrUnavailable):
		code = codes.Unavailable
	}

	return status.Error(code, err.Error())
}

// WrapNotFound wraps an error with ErrNotFound context.
func WrapNotFound(msg string) error {
	return fmt.Errorf("%w: %s", ErrNotFound, msg)
}

// WrapInvalidArgument wraps an error with ErrInvalidArgument context.
func WrapInvalidArgument(msg string) error {
	return fmt.Errorf("%w: %s", ErrInvalidArgument, msg)
}

// WrapInternal wraps an error with ErrInternal context.
func WrapInternal(msg string, err error) error {
	return fmt.Errorf("%w: %s: %v", ErrInternal, msg, err)
}
