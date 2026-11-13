package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-fuego/fuego"

	"github.com/mrRedSun/redrop/internal/apperr"
)

var statusByKind = map[apperr.Kind]int{
	apperr.KindInternal:     http.StatusInternalServerError,
	apperr.KindInvalid:      http.StatusBadRequest,
	apperr.KindUnauthorized: http.StatusUnauthorized,
	apperr.KindForbidden:    http.StatusForbidden,
	apperr.KindNotFound:     http.StatusNotFound,
	apperr.KindConflict:     http.StatusConflict,
	apperr.KindGone:         http.StatusGone,
	apperr.KindTooLarge:     http.StatusRequestEntityTooLarge,
	apperr.KindRateLimited:  http.StatusTooManyRequests,
}

// StatusOf returns the HTTP status for an application error kind.
func StatusOf(k apperr.Kind) int { return statusByKind[k] }

// toProblem converts any error into RFC 9457 problem details. Fuego's own
// errors (validation, decoding) pass through; internal causes are logged and
// never exposed.
func toProblem(ctx context.Context, err error) fuego.HTTPError {
	var he fuego.HTTPError
	if errors.As(err, &he) {
		return he
	}
	var ws fuego.ErrorWithStatus
	if errors.As(err, &ws) {
		return fuego.HandleHTTPError(ctx, err).(fuego.HTTPError)
	}
	e := apperr.As(err)
	status := StatusOf(e.Kind)
	if e.Kind == apperr.KindInternal {
		slog.ErrorContext(ctx, "internal error", "error", err)
	}
	title := e.Code
	if title == "" {
		title = http.StatusText(status)
	}
	return fuego.HTTPError{Status: status, Title: title, Detail: e.Msg}
}

// writeError renders an error from a plain net/http handler.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	fuego.SendJSONError(w, r, toProblem(r.Context(), err))
}
