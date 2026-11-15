package content

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/internal/sharing"
)

// feedbackPath is where the viewer posts comments (DESIGN.md "Viewer
// feedback"). Only POST is routed there, and only while the artifact takes
// comments; otherwise it answers like any other reserved path.
const feedbackPath = reservedPrefix + "feedback"

// maxFeedbackRequest bounds a post's body. The largest valid post, every
// field at its limit in astral characters and every character escaped as a
// JSON \u surrogate pair, is about 63 KB; browsers send raw UTF-8, about a
// third of that.
const maxFeedbackRequest = 64 << 10

// feedbackRequest is the endpoint's JSON body.
type feedbackRequest struct {
	Body    string `json:"body"`
	Name    string `json:"name"`
	Version int    `json:"version"`
	Page    string `json:"page"`
	Quote   *struct {
		Exact  string `json:"exact"`
		Prefix string `json:"prefix"`
		Suffix string `json:"suffix"`
	} `json:"quote"`
}

// feedback answers POST /__redrop/feedback for slug. Until the artifact is
// known to take comments, every answer is what a POST to any other reserved
// path gets, so a closed endpoint can't be told apart from an unknown path;
// a slug that doesn't resolve to an available artifact also spends miss
// budget, like every miss. The checks then run cheapest and most general
// first: origin, content type, access, the per-client limit, the body.
// Posting never records or consumes a view.
func (h *Handler) feedback(w http.ResponseWriter, r *http.Request, slug string) {
	a, err := h.sharing.Lookup(r.Context(), slug)
	d := sharing.ShowGone
	if err == nil {
		d = h.sharing.Decide(a, sharing.Grants{Visit: h.cookieValue(r, cookieVisit), Preview: h.cookieValue(r, cookiePreview)}, time.Now())
	}
	switch {
	case d == sharing.ShowGone:
		if !h.misses.Take(h.clients.Network(r)) {
			h.tooManyMisses(w)
			return
		}
		methodNotAllowed(w)
		return
	case !h.sharing.FeedbackOpen(a):
		methodNotAllowed(w)
		return
	case !sameOriginPost(r, h.selfOrigin(a)):
		feedbackAnswer(w, http.StatusForbidden, "cross_origin", "Feedback can only be sent from this page.")
		return
	case !jsonContent(r):
		feedbackAnswer(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Send feedback as JSON.")
		return
	case d == sharing.ShowGate:
		feedbackAnswer(w, http.StatusForbidden, "access_required", "Your access to this page has ended. Reload it to continue.")
		return
	case !h.posts.Take(h.clients.Network(r)):
		w.Header().Set("Retry-After", strconv.Itoa((60+h.cfg.FeedbackPerMinute-1)/h.cfg.FeedbackPerMinute))
		feedbackAnswer(w, http.StatusTooManyRequests, "rate_limited", "Too much feedback from your network. Wait a minute and try again.")
		return
	}
	req, status := decodeFeedback(w, r)
	if status != 0 {
		feedbackAnswer(w, status, "invalid_request", "The feedback could not be read.")
		return
	}
	post := sharing.FeedbackPost{Body: req.Body, Name: req.Name, Version: req.Version, Page: req.Page}
	if q := req.Quote; q != nil {
		post.Quote = &sharing.TextQuote{Exact: q.Exact, Prefix: q.Prefix, Suffix: q.Suffix}
	}
	if err := h.sharing.PostFeedback(r.Context(), a, post); err != nil {
		switch e := apperr.As(err); e.Kind {
		case apperr.KindInvalid:
			feedbackAnswer(w, http.StatusBadRequest, "invalid", e.Msg)
		case apperr.KindConflict:
			feedbackAnswer(w, http.StatusConflict, "feedback_full", e.Msg)
		default:
			h.log.Error("post feedback", "error", err)
			feedbackAnswer(w, http.StatusInternalServerError, "internal", "Something went wrong. Please try again.")
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	w.Write([]byte(`{"ok":true}` + "\n"))
}

// decodeFeedback reads one bounded JSON object with only the known fields.
// It returns a non-zero status when the body can't be used.
func decodeFeedback(w http.ResponseWriter, r *http.Request) (feedbackRequest, int) {
	var req feedbackRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFeedbackRequest))
	dec.DisallowUnknownFields()
	err := dec.Decode(&req)
	if err == nil && dec.Decode(&struct{}{}) != io.EOF {
		err = errors.New("trailing data")
	}
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		return req, http.StatusRequestEntityTooLarge
	case err != nil:
		return req, http.StatusBadRequest
	}
	return req, 0
}

// sameOriginPost reports a request sent from self, the artifact's own
// origin: Origin must name it (browsers send Origin with every POST), and
// Fetch Metadata, when present, must agree. A default port in self is not
// part of the origin browsers send.
func sameOriginPost(r *http.Request, self string) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		return false
	}
	return r.Header.Get("Origin") == withoutDefaultPort(self)
}

func withoutDefaultPort(origin string) string {
	u, err := url.Parse(origin)
	if err != nil {
		return origin
	}
	if (u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80") {
		return u.Scheme + "://" + u.Hostname()
	}
	return origin
}

// jsonContent reports a JSON request body (parameters such as charset allowed).
func jsonContent(r *http.Request) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && strings.EqualFold(mt, "application/json")
}

// feedbackAnswer writes the endpoint's JSON error. It never carries CORS
// headers, so no other origin can read it.
func feedbackAnswer(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": code, "message": msg})
}
