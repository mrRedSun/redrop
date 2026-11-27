package sharing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/internal/ids"
	"github.com/mrRedSun/redrop/internal/notify"
	"github.com/mrRedSun/redrop/internal/store"
	"github.com/mrRedSun/redrop/pkg/api"
)

// Abuse reports (DESIGN.md "Abuse handling"): anyone may report a link
// through the form on the app origin; administrators work through the queue
// on the Moderation port. A report never acts on its own.

// Report categories.
const (
	CategoryPhishing = "phishing"
	CategoryMalware  = "malware"
	CategoryIllegal  = "illegal"
	CategorySpam     = "spam"
	CategoryOther    = "other"
)

// ReportCategories lists the categories a visitor may choose.
var ReportCategories = []string{CategoryPhishing, CategoryMalware, CategoryIllegal, CategorySpam, CategoryOther}

// Report limits, in Unicode code points except the URL (bytes).
const (
	maxReportURL     = 2048
	maxReportDetails = 2000
	maxReportContact = 200
	maxReportList    = 50 // one page of the admin queue
)

// ReportInput is a report as a visitor files it. Every field is untrusted.
type ReportInput struct {
	URL      string
	Category string
	Details  string
	Contact  string
}

// SetNotifier sets where new reports are announced. Call it before serving;
// without it, nothing is sent.
func (s *Service) SetNotifier(n notify.Notifier) { s.notifier = n }

// ReportAnnouncer is the mail side of new-report announcements: the
// batched report alerts (DESIGN.md "Email"). It gets the report ID only.
type ReportAnnouncer interface {
	Notify(reportID string)
}

// SetAlertNotifier wires the batched report alerts. Call it before serving.
func (s *Service) SetAlertNotifier(a ReportAnnouncer) { s.alerts = a }

// SubmitReport files a visitor's report from the client network. Rate
// limits, validation and the global cap answer the same whatever the URL
// names; past them the answer is always success, whether the report was
// stored or dropped (an unknown link, a full artifact, or a duplicate from
// the same network), so the form can't be used to find links.
func (s *Service) SubmitReport(ctx context.Context, network string, in ReportInput) error {
	if !s.reportLimiter.Allow(network) {
		return apperr.RateLimited("Too many reports from your network. Try again later.").WithRetryAfter(s.reportLimiter.Wait(network))
	}
	slug, link, err := s.reportedLink(in.URL)
	if err != nil {
		return err
	}
	if !slices.Contains(ReportCategories, in.Category) {
		return apperr.Invalid("Choose a category.")
	}
	details := strings.TrimSpace(cleanText(in.Details, true))
	if utf8.RuneCountInString(details) > maxReportDetails {
		return apperr.Invalid("Describe the problem in up to %d characters.", maxReportDetails)
	}
	contact := strings.TrimSpace(cleanText(in.Contact, false))
	if utf8.RuneCountInString(contact) > maxReportContact {
		return apperr.Invalid("The contact can have up to %d characters.", maxReportContact)
	}
	if n, err := s.db.CountOpenReports(ctx, store.ReportVisitor); err != nil {
		return apperr.Internal(err)
	} else if n >= s.cfg.ReportsMaxOpen {
		return s.queueFull()
	}
	a, err := s.Lookup(ctx, slug)
	if err != nil {
		return nil // unknown link: dropped, answered like a stored report
	}
	// Only artifacts the content host would serve can be reported: a revoked,
	// expired or exhausted link is already unavailable to visitors, and
	// storing (or refusing) a report for one would turn the form into an
	// oracle for which hidden links still exist.
	if a.Status(time.Now()) != api.StatusActive {
		return nil
	}
	r := &store.Report{ArtifactID: a.ID, OwnerID: a.UserID, URL: link, Category: in.Category, Details: details, Contact: contact, Network: s.networkKey(network)}
	stored, err := s.db.AddVisitorReport(ctx, r, s.cfg.ReportsMaxOpen, s.cfg.ReportsMaxOpenPerArtifact)
	if err != nil {
		return apperr.Internal(err)
	}
	if stored {
		s.log.Info("abuse report", "report", r.ID, "id", a.ID, "category", r.Category)
		s.announce(r, a.OwnerUsername)
	}
	return nil
}

func (s *Service) queueFull() error {
	msg := "This server isn't accepting reports right now."
	if c, ok := strings.CutPrefix(s.cfg.AbuseContact, "mailto:"); ok {
		msg += " Write to " + c + " instead."
	} else if s.cfg.AbuseContact != "" {
		msg += " Use " + s.cfg.AbuseContact + " instead."
	}
	return apperr.RateLimited("%s", msg)
}

// reportedLink checks content-host and configured app-origin intake URLs by
// syntax only. Content URLs keep their path and query, without a fragment;
// intake URLs become canonical content URLs, without a query or fragment.
func (s *Service) reportedLink(raw string) (slug, link string, err error) {
	bad := apperr.Invalid("Enter the address of a page shared from this server, like %s.", s.cfg.ArtifactURL("amber-otter-quilt-river"))
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxReportURL {
		return "", "", bad
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Opaque != "" {
		return "", "", bad
	}
	if s.cfg.PublicURL != nil && u.Scheme == s.cfg.PublicURL.Scheme && strings.EqualFold(u.Host, s.cfg.PublicURL.Host) {
		path, ok := strings.CutPrefix(u.EscapedPath(), api.E2EIntakePrefix)
		slug := strings.TrimSuffix(path, "/")
		if !ok || !ids.ValidSlug(slug) {
			return "", "", bad
		}
		return slug, s.cfg.ArtifactURL(slug), nil
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	label, ok := strings.CutSuffix(host, "."+s.cfg.ContentHost())
	if !ok || !ids.ValidSlug(label) {
		return "", "", bad
	}
	u.Fragment, u.RawFragment = "", ""
	port := u.Port()
	u.Host = host
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	}
	return label, u.String(), nil
}

// networkKey is the keyed hash of a client network that a report keeps
// while it is open, for the duplicate check. The address itself is never
// stored.
func (s *Service) networkKey(network string) string {
	m := hmac.New(sha256.New, s.reportKey)
	m.Write([]byte(network))
	return hex.EncodeToString(m.Sum(nil))
}

// announce sends the webhook notification of a stored report: category,
// artifact ID and owner only. Report text, contacts, links and file names
// are untrusted and never leave the server this way.
func (s *Service) announce(r *store.Report, owner string) {
	title := "redrop: abuse report"
	if r.Source == store.ReportSystem {
		title = "redrop: upload flagged"
	}
	s.notifier.Notify(notify.Message{
		Level:   "warning",
		Title:   title,
		Message: fmt.Sprintf("Category %s, artifact %s, owner %s.", r.Category, r.ArtifactID, owner),
		Key:     "redrop-report-" + r.ID,
	})
	if s.alerts != nil {
		s.alerts.Notify(r.ID)
	}
}

// --- the admin queue ---

// AdminReport is a report as the queue shows it. Like AdminArtifact it
// carries no link to the artifact, with one exception: ReportedURL, the
// link a visitor submitted with the report (DESIGN.md "Admin queue").
type AdminReport struct {
	ID          string
	Source      string // visitor | system
	Category    string
	Details     string // untrusted: the reporter's text, or the finding
	Contact     string // untrusted, unverified
	ReportedURL string // visitor reports: the normalized, fragment-free link; "" for system reports
	Version     int    // system reports: the flagged version
	Status      string
	CreatedAt   time.Time
	ClosedAt    *time.Time
	ClosedBy    string

	ArtifactID     string
	Artifact       *AdminArtifact // nil once the artifact is deleted
	OwnerUsername  string
	OwnerSuspended bool // the owner's account is disabled
	History        []AuditEntry
}

// AuditEntry is one action on a report.
type AuditEntry struct {
	At     time.Time
	Actor  string
	Action string
	Detail string
}

// ReportListPage is one page of the admin queue.
type ReportListPage struct {
	Reports  []AdminReport
	Total    int // for the active status and source filter
	Page     int // 1-based
	PageSize int
}

// Reports lists one page of the queue: status is open (the default),
// resolved, dismissed or all; source is visitor, system or all (the
// default). With both sources shown, visitor reports come first, then
// newest first within each source, so a flood of upload flags cannot bury
// a visitor's report. page is 1-based; out-of-range pages are empty.
func (m *Moderation) Reports(ctx context.Context, status, source string, page int) (ReportListPage, error) {
	switch status {
	case "":
		status = store.ReportOpen
	case "all":
		status = ""
	case store.ReportOpen, store.ReportResolved, store.ReportDismissed:
	default:
		return ReportListPage{}, apperr.Invalid("status must be open, resolved, dismissed or all")
	}
	switch source {
	case "", "all":
		source = ""
	case store.ReportVisitor, store.ReportSystem:
	default:
		return ReportListPage{}, apperr.Invalid("source must be visitor, system or all")
	}
	if page < 1 {
		page = 1
	}
	rows, total, err := m.s.db.ListReports(ctx, status, source, maxReportList, (page-1)*maxReportList)
	if err != nil {
		return ReportListPage{}, apperr.Internal(err)
	}
	views, err := m.reportViews(ctx, rows)
	if err != nil {
		return ReportListPage{}, err
	}
	return ReportListPage{Reports: views, Total: total, Page: page, PageSize: maxReportList}, nil
}

func (m *Moderation) reportViews(ctx context.Context, rows []store.Report) ([]AdminReport, error) {
	artifactIDs := make([]string, 0, len(rows))
	for _, r := range rows {
		artifactIDs = append(artifactIDs, r.ArtifactID)
	}
	slices.Sort(artifactIDs)
	arts, err := m.s.db.ArtifactsByIDs(ctx, slices.Compact(artifactIDs))
	if err != nil {
		return nil, apperr.Internal(err)
	}
	hist, err := m.s.db.AuditFor(ctx, rows)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	out := make([]AdminReport, 0, len(rows))
	for _, r := range rows {
		v := AdminReport{ID: r.ID, Source: r.Source, Category: r.Category, Details: r.Details, Contact: r.Contact, ReportedURL: r.URL,
			Version: r.Version, Status: r.Status, CreatedAt: r.CreatedAt, ClosedAt: r.ClosedAt, ClosedBy: r.ClosedBy,
			ArtifactID: r.ArtifactID, OwnerUsername: r.OwnerUsername, OwnerSuspended: r.OwnerDisabled, History: []AuditEntry{}}
		if a, ok := arts[r.ArtifactID]; ok {
			av := m.s.adminView(a)
			v.Artifact = &av
		}
		for _, e := range hist[r.ID] {
			v.History = append(v.History, AuditEntry{At: e.At, Actor: e.Actor, Action: e.Action, Detail: e.Detail})
		}
		out = append(out, v)
	}
	return out, nil
}

func (m *Moderation) report(ctx context.Context, id string) (AdminReport, error) {
	r, err := m.s.db.ReportByID(ctx, id)
	if err != nil {
		return AdminReport{}, reportErr(err)
	}
	views, err := m.reportViews(ctx, []store.Report{*r})
	if err != nil {
		return AdminReport{}, err
	}
	return views[0], nil
}

// SetReportStatus resolves, dismisses or reopens a report.
func (m *Moderation) SetReportStatus(ctx context.Context, admin, id, status string) (AdminReport, error) {
	if status != store.ReportOpen && status != store.ReportResolved && status != store.ReportDismissed {
		return AdminReport{}, apperr.Invalid("status must be open, resolved or dismissed")
	}
	if err := m.s.db.SetReportStatus(ctx, id, status, admin); err != nil {
		return AdminReport{}, reportErr(err)
	}
	m.s.log.Info("admin changed report", "admin", admin, "report", id, "status", status)
	return m.report(ctx, id)
}

// RevokeReported revokes the reported artifact as an administrator (the
// owner can't undo it) and resolves its open reports.
func (m *Moderation) RevokeReported(ctx context.Context, admin, id string) (AdminReport, error) {
	r, err := m.s.db.ReportByID(ctx, id)
	if err != nil {
		return AdminReport{}, reportErr(err)
	}
	detail, err := m.s.db.RevokeReported(ctx, id, admin)
	if err != nil {
		return AdminReport{}, reportErr(err)
	}
	// The queue revokes straight in the store, so the registry hears about
	// it here rather than through syncTunnel.
	m.s.syncTunnel(ctx, r.ArtifactID)
	m.s.log.Info("admin revoked reported artifact", "admin", admin, "report", id, "detail", detail)
	return m.report(ctx, id)
}

// SuspendReportedOwner suspends the reported artifact's owner: the account
// is disabled (no sign-in, no API tokens, so no publishing), every artifact
// it owns is revoked as an administrator, and their open reports are
// resolved. Administrator accounts are refused.
func (m *Moderation) SuspendReportedOwner(ctx context.Context, admin, id string) (AdminReport, error) {
	detail, err := m.s.db.SuspendReported(ctx, id, admin)
	if err != nil {
		return AdminReport{}, reportErr(err)
	}
	// The suspension disables the owner directly in the store, so their
	// tunnels are ended here rather than through the accounts service.
	if m.s.tunnels != nil {
		if r, err := m.s.db.ReportByID(ctx, id); err == nil {
			m.s.tunnels.EndUser(r.OwnerID)
		}
	}
	m.s.log.Info("admin suspended reported owner", "admin", admin, "report", id, "detail", detail)
	return m.report(ctx, id)
}

func reportErr(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return apperr.NotFound("report not found")
	case errors.Is(err, store.ErrArtifactGone):
		return apperr.Conflict("the reported artifact was deleted")
	case errors.Is(err, store.ErrOwnerIsAdmin):
		return apperr.Conflict("the owner is an administrator; change their role on the Users page before suspending them")
	}
	return apperr.Internal(err)
}
