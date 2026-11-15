package sharing

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/internal/store"
	"github.com/mrRedSun/redrop/pkg/api"
)

// Viewer feedback (DESIGN.md "Viewer feedback"): visitors who can view an
// artifact may leave comments while its owner allows it; only the owner
// reads them. Comments are untrusted text and are stored without any client
// detail (no IP address, user agent or cookie).

// Comment limits, in Unicode code points except the page (bytes).
const (
	maxCommentBody  = 4000
	maxCommentName  = 80
	maxCommentPage  = 2048
	maxQuoteExact   = 500
	maxQuoteContext = 64
)

// TextQuote anchors a comment to selected text: a W3C TextQuoteSelector.
type TextQuote struct {
	Exact  string
	Prefix string
	Suffix string
}

// FeedbackPost is a comment as a visitor sends it.
type FeedbackPost struct {
	Body    string
	Name    string
	Version int    // the version on screen
	Page    string // pathname + search + hash inside the artifact
	Quote   *TextQuote
}

// Comment is a stored comment as the owner sees it.
type Comment struct {
	ID         string
	Version    int
	Page       string
	Body       string
	Name       string
	Quote      *TextQuote // nil without a text anchor
	CreatedAt  time.Time
	ResolvedAt *time.Time
}

// FeedbackOpen reports whether a takes comments: the owner turned feedback
// on, and the info panel that holds the form is shown.
func (s *Service) FeedbackOpen(a *store.Artifact) bool { return a.Feedback && s.infoButton(a) }

// PostFeedback stores a visitor's comment on a. The caller has already
// decided that the visitor may view a (sharing.Decide); posting never counts
// or consumes a view.
func (s *Service) PostFeedback(ctx context.Context, a *store.Artifact, p FeedbackPost) error {
	if !s.FeedbackOpen(a) {
		return apperr.NotFound("not found")
	}
	c, err := validComment(a, p)
	if err != nil {
		return err
	}
	if err := s.db.AddComment(ctx, c, s.cfg.FeedbackMaxPerArtifact); err != nil {
		if errors.Is(err, store.ErrFeedbackFull) {
			return apperr.Conflict("This page isn't accepting more feedback.").WithCode("feedback_full")
		}
		return apperr.Internal(err)
	}
	s.log.Info("feedback posted", "id", a.ID, "comment", c.ID, "version", c.Version)
	return nil
}

// validComment checks and normalizes a post into a row for a.
func validComment(a *store.Artifact, p FeedbackPost) (*store.Comment, error) {
	body := strings.TrimSpace(cleanText(p.Body, true))
	if n := utf8.RuneCountInString(body); n == 0 || n > maxCommentBody {
		return nil, apperr.Invalid("Write a comment of up to %d characters.", maxCommentBody)
	}
	name := strings.TrimSpace(cleanText(p.Name, false))
	if utf8.RuneCountInString(name) > maxCommentName {
		return nil, apperr.Invalid("The name can have up to %d characters.", maxCommentName)
	}
	if p.Version < 1 || p.Version > max(a.LastVersion, a.CurrentVersion) {
		return nil, apperr.Invalid("Unknown version.")
	}
	if !validPage(p.Page) {
		return nil, apperr.Invalid("Invalid page.")
	}
	c := &store.Comment{ArtifactID: a.ID, Version: p.Version, Page: p.Page, Body: body, Name: name}
	if q := p.Quote; q != nil {
		exact, prefix, suffix := cleanText(q.Exact, true), cleanText(q.Prefix, true), cleanText(q.Suffix, true)
		switch {
		case exact == "" && (prefix != "" || suffix != ""):
			return nil, apperr.Invalid("A quote needs its text.")
		case utf8.RuneCountInString(exact) > maxQuoteExact:
			return nil, apperr.Invalid("The quote can have up to %d characters.", maxQuoteExact)
		case utf8.RuneCountInString(prefix) > maxQuoteContext || utf8.RuneCountInString(suffix) > maxQuoteContext:
			return nil, apperr.Invalid("The text around the quote can have up to %d characters.", maxQuoteContext)
		}
		c.QuoteExact, c.QuotePrefix, c.QuoteSuffix = exact, prefix, suffix
	}
	return c, nil
}

// validPage accepts a path inside the artifact as the address bar shows it:
// exactly one leading slash, no control characters, outside redrop's own
// path space, and bounded.
func validPage(p string) bool {
	if len(p) > maxCommentPage || !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.HasPrefix(p, `/\`) || !utf8.ValidString(p) {
		return false
	}
	path, _, _ := strings.Cut(p, "?")
	path, _, _ = strings.Cut(path, "#")
	if path == "/"+api.ReservedPrefix || strings.HasPrefix(path, "/"+api.ReservedPrefix+"/") {
		return false
	}
	return strings.IndexFunc(p, unicode.IsControl) < 0
}

// cleanText makes viewer text valid UTF-8 without control characters (C0,
// DEL, C1). Multiline text keeps line feeds and tabs, with \r\n and lone \r
// as \n; single-line text has none.
func cleanText(s string, multiline bool) string {
	s = strings.ToValidUTF8(s, "�")
	if multiline {
		s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	}
	return strings.Map(func(r rune) rune {
		if multiline && (r == '\n' || r == '\t') {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// FeedbackFilter selects the owner's view of comments.
type FeedbackFilter struct {
	Version int    // 0 = every version
	Status  string // "open", or "" / "all" for every comment
}

// Feedback lists the comments on the actor's artifact, oldest first.
func (s *Service) Feedback(ctx context.Context, actor Actor, id string, f FeedbackFilter) ([]Comment, error) {
	a, err := s.owned(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	sf := store.CommentFilter{Version: f.Version}
	switch f.Status {
	case "", "all":
	case "open":
		sf.OpenOnly = true
	default:
		return nil, apperr.Invalid("status must be open or all")
	}
	if f.Version < 0 {
		return nil, apperr.Invalid("invalid version")
	}
	rows, err := s.db.ListComments(ctx, a.ID, sf)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	out := make([]Comment, 0, len(rows))
	for _, r := range rows {
		out = append(out, commentView(r))
	}
	return out, nil
}

// ResolveComment marks a comment on the actor's artifact resolved or open.
func (s *Service) ResolveComment(ctx context.Context, actor Actor, id, commentID string, resolved bool) (Comment, error) {
	a, err := s.owned(ctx, actor, id)
	if err != nil {
		return Comment{}, err
	}
	c, err := s.db.SetCommentResolved(ctx, a.ID, commentID, resolved)
	if err != nil {
		return Comment{}, commentErr(err)
	}
	return commentView(*c), nil
}

// DeleteComment deletes a comment on the actor's artifact.
func (s *Service) DeleteComment(ctx context.Context, actor Actor, id, commentID string) error {
	a, err := s.owned(ctx, actor, id)
	if err != nil {
		return err
	}
	return commentErr(s.db.DeleteComment(ctx, a.ID, commentID))
}

func commentErr(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return apperr.NotFound("comment not found")
	}
	return mapStore(err)
}

func commentView(r store.Comment) Comment {
	c := Comment{ID: r.ID, Version: r.Version, Page: r.Page, Body: r.Body, Name: r.Name, CreatedAt: r.CreatedAt, ResolvedAt: r.ResolvedAt}
	if r.QuoteExact != "" {
		c.Quote = &TextQuote{Exact: r.QuoteExact, Prefix: r.QuotePrefix, Suffix: r.QuoteSuffix}
	}
	return c
}
