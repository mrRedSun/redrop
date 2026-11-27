package httpapi

import (
	"net/http"
	"strconv"

	"github.com/go-fuego/fuego"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/pkg/api"
)

// Abuse reports (DESIGN.md "Abuse handling").

// maxReportBody bounds a report: a 2048-byte URL and 2,200 characters of
// text, which are up to 4 bytes each in UTF-8, plus the JSON envelope.
const maxReportBody = 12 << 10

// submitReport files a visitor's report. The answer is the same whether or
// not the link exists; see sharing.SubmitReport.
func (a *API) submitReport(c fuego.ContextWithBody[api.ReportRequest]) (api.OK, error) {
	body, err := c.Body()
	if err != nil {
		return api.OK{}, err
	}
	if err := a.sharing.SubmitReport(c.Context(), a.clients.Network(c.Request()), reportFromRequest(body)); err != nil {
		return api.OK{}, err
	}
	return api.OK{OK: true}, nil
}

// sameOriginOnly admits a request only from a page of the app origin: the
// CSRF header, Fetch Metadata when sent, and an Origin header, required,
// that names the app origin. The report form has no cookie to protect, but
// without it any website could file reports from its visitors' networks.
func (a *API) sameOriginOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := a.checkCSRF(r)
		if err == nil && r.Header.Get("Origin") != a.cfg.PublicOrigin() {
			err = apperr.Forbidden("reports can only be sent from this server's report page")
		}
		if err != nil {
			writeError(w, r, err)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// reportBody admits a report's JSON body, up to maxReportBody.
func (a *API) reportBody(next http.Handler) http.Handler { return jsonBody(next, maxReportBody) }

func (h adminAPI) listReports(c fuego.ContextNoBody) (api.ReportList, error) {
	page := 1
	if raw := c.QueryParam("page"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return api.ReportList{}, apperr.Invalid("page must be a positive integer")
		}
		page = n
	}
	list, err := h.moderation.Reports(c.Context(), c.QueryParam("status"), c.QueryParam("source"), page)
	if err != nil {
		return api.ReportList{}, err
	}
	return api.ReportList{Items: mapItems(list.Reports, reportDTO), Total: list.Total, Page: list.Page, PageSize: list.PageSize}, nil
}

func (h adminAPI) updateReport(c fuego.ContextWithBody[api.ReportStatusUpdate]) (api.Report, error) {
	body, err := c.Body()
	if err != nil {
		return api.Report{}, err
	}
	r, err := h.moderation.SetReportStatus(c.Context(), principalFrom(c.Context()).Username(), c.PathParam("id"), body.Status)
	if err != nil {
		return api.Report{}, err
	}
	return reportDTO(r), nil
}

func (h adminAPI) revokeReported(c fuego.ContextNoBody) (api.Report, error) {
	r, err := h.moderation.RevokeReported(c.Context(), principalFrom(c.Context()).Username(), c.PathParam("id"))
	if err != nil {
		return api.Report{}, err
	}
	return reportDTO(r), nil
}

func (h adminAPI) suspendReportedOwner(c fuego.ContextNoBody) (api.Report, error) {
	r, err := h.moderation.SuspendReportedOwner(c.Context(), principalFrom(c.Context()).Username(), c.PathParam("id"))
	if err != nil {
		return api.Report{}, err
	}
	return reportDTO(r), nil
}
