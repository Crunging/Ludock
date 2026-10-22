// Package apperr defines errors whose messages are deliberately public.
package apperr

import (
	"errors"
	"fmt"
)

// Error is an error that may cross the HTTP boundary. Its message must never
// contain secrets, host paths, or Docker identifiers.
type Error struct {
	Code    string
	Status  int
	Message string
	// RetryAfterMs, when positive, becomes a Retry-After header.
	RetryAfterMs int64
}

func (e *Error) Error() string { return e.Message }

// New builds a public error.
func New(code string, status int, message string) *Error {
	return &Error{Code: code, Status: status, Message: message}
}

// Newf builds a public error with a formatted message.
func Newf(code string, status int, format string, args ...any) *Error {
	return &Error{Code: code, Status: status, Message: fmt.Sprintf(format, args...)}
}

// As extracts a public error.
func As(err error) (*Error, bool) {
	var target *Error
	ok := errors.As(err, &target)
	return target, ok
}

// Is reports whether err is a public error with code.
func Is(err error, code string) bool {
	target, ok := As(err)
	return ok && target.Code == code
}

// Public returns the message and code that may be shown for any error.
func Public(err error) (message, code string) {
	if target, ok := As(err); ok {
		return target.Message, target.Code
	}
	return "The operation failed. Check administrator diagnostics.", "OPERATION_FAILED"
}
