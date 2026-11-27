package content

import (
	"net/http"
	"strings"
	"time"

	"github.com/mrRedSun/redrop/internal/config"
	"github.com/mrRedSun/redrop/internal/dashpaths"
)

// SecurityTxtPath is where RFC 9116 puts the file.
const SecurityTxtPath = "/.well-known/security.txt"

// securityTxtLifetime is how far ahead Expires is set. RFC 9116 recommends
// less than a year; the file is generated per request, so it never goes
// stale.
const securityTxtLifetime = 180 * 24 * time.Hour

// SecurityTxt serves /.well-known/security.txt for the host whose origin is
// canonical (the app origin or the content apex): the operator's
// REDROP_ABUSE_CONTACT and the report form (DESIGN.md "Abuse handling").
// Without a configured contact it is 404.
func SecurityTxt(cfg *config.Config, canonical string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.AbuseContact == "" {
			http.NotFound(w, r)
			return
		}
		var b strings.Builder
		b.WriteString("# Security and abuse contact for this redrop server.\n")
		b.WriteString("Contact: " + cfg.AbuseContact + "\n")
		b.WriteString("# Report a shared link that is abusive: phishing, malware, illegal content, spam.\n")
		b.WriteString("Contact: " + cfg.PublicOrigin() + dashpaths.Report + "\n")
		b.WriteString("Expires: " + time.Now().Add(securityTxtLifetime).UTC().Format(time.RFC3339) + "\n")
		b.WriteString("Preferred-Languages: en\n")
		b.WriteString("Canonical: " + canonical + SecurityTxtPath + "\n")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write([]byte(b.String()))
	}
}
