package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"

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
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return fuego.HTTPError{Status: http.StatusRequestEntityTooLarge, Title: http.StatusText(http.StatusRequestEntityTooLarge), Detail: "request body is too large"}
	}
	var he fuego.HTTPError
	if errors.As(err, &he) {
		// Fuego's decoding errors wrap an HTTPError without a status; the
		// wrapper (BadRequestError) carries it.
		var ws fuego.ErrorWithStatus
		if he.Status == 0 && errors.As(err, &ws) {
			he.Status = ws.StatusCode()
		}
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
	// Err is never serialized; it lets sendProblem read RetryAfter.
	return fuego.HTTPError{Status: status, Title: title, Detail: e.Msg, Err: e}
}

// setRetryAfter sets Retry-After (whole seconds, rounded up) for a
// rate-limited error that knows its wait.
func setRetryAfter(w http.ResponseWriter, err error) {
	var e *apperr.Error
	if errors.As(err, &e) && e.Kind == apperr.KindRateLimited && e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(e.RetryAfter.Seconds()))))
	}
}

// sendProblem serializes every Fuego route's error (after toProblem): the
// usual negotiated problem response, plus Retry-After on a rate limit that
// knows its wait.
func sendProblem(w http.ResponseWriter, r *http.Request, err error) {
	setRetryAfter(w, err)
	fuego.SendError(w, r, err)
}

// writeError renders an error from a plain net/http handler.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	p := toProblem(r.Context(), err)
	setRetryAfter(w, p)
	fuego.SendJSONError(w, r, p)
}
