package httpapi

import (
	"net/http"

	"github.com/go-fuego/fuego"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/pkg/api"
)

// Abuse reports (DESIGN.md "Abuse handling").

// maxReportBody bounds a report: a 2048-byte URL and 2,200 characters of
// text, with room for JSON escaping.
const maxReportBody = 8 << 10

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
	list, err := h.moderation.Reports(c.Context(), c.QueryParam("status"))
	if err != nil {
		return api.ReportList{}, err
	}
	return api.ReportList{Items: mapItems(list, reportDTO)}, nil
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
