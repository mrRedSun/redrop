// Package apperr defines typed application errors. Services return them;
// transports (HTTP API, content pages) map the Kind to a response once.
package apperr

import (
	"errors"
	"fmt"
)

// Kind classifies an error independently of any transport.
type Kind int

const (
	KindInternal Kind = iota
	KindInvalid
	KindUnauthorized
	KindForbidden
	KindNotFound
	KindConflict
	KindGone
	KindTooLarge
	KindRateLimited
)

// Error is a user-safe error with a kind, a message, and an optional
// machine-readable code (e.g. "totp_required").
type Error struct {
	Kind Kind
	Code string
	Msg  string
	Err  error // internal cause, never shown to users
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Msg + ": " + e.Err.Error()
	}
	return e.Msg
}

func (e *Error) Unwrap() error { return e.Err }

func newf(k Kind, format string, a ...any) *Error {
	return &Error{Kind: k, Msg: fmt.Sprintf(format, a...)}
}

func Invalid(format string, a ...any) *Error      { return newf(KindInvalid, format, a...) }
func Unauthorized(format string, a ...any) *Error { return newf(KindUnauthorized, format, a...) }
func Forbidden(format string, a ...any) *Error    { return newf(KindForbidden, format, a...) }
func NotFound(format string, a ...any) *Error     { return newf(KindNotFound, format, a...) }
func Conflict(format string, a ...any) *Error     { return newf(KindConflict, format, a...) }
func Gone(format string, a ...any) *Error         { return newf(KindGone, format, a...) }
func TooLarge(format string, a ...any) *Error     { return newf(KindTooLarge, format, a...) }
func RateLimited(format string, a ...any) *Error  { return newf(KindRateLimited, format, a...) }

// WithCode attaches a machine-readable code.
func (e *Error) WithCode(code string) *Error { e.Code = code; return e }

// Internal wraps an unexpected error; its cause is logged, not shown.
func Internal(err error) *Error { return &Error{Kind: KindInternal, Msg: "internal error", Err: err} }

// As extracts an *Error, treating anything else as internal.
func As(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return Internal(err)
}

// KindOf returns the kind of err (KindInternal for foreign errors).
func KindOf(err error) Kind {
	if err == nil {
		return KindInternal
	}
	return As(err).Kind
}

// Is reports whether err has the given kind.
func Is(err error, k Kind) bool {
	var e *Error
	return errors.As(err, &e) && e.Kind == k
}
