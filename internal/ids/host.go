package ids

import (
	"strconv"
	"strings"
)

// Host is a parsed artifact host label: which artifact, and which of its
// origins. It is the only place the label grammar lives; content routing,
// URL building and the README's reverse-proxy rule all follow it.
//
//	<slug>          viewer origin: the shared link (redrop's viewer page, gates)
//	<slug>--raw     raw origin of the current version (stable across updates)
//	<slug>--v<N>    raw origin pinned to version N (1..MaxHostVersion, no leading zero)
//
// Valid slugs never contain "--" (the word format uses single hyphens, the
// legacy format none), so the first "--" always starts the suffix. The
// longest label is MaxSlugLen + len("--v999999") = 48 octets.
type Host struct {
	slug    string
	raw     bool
	version int // raw only; 0 = current
}

// MaxHostVersion bounds the version digits in a raw label.
const MaxHostVersion = 999_999

const hostSep = "--"

// ParseHostLabel parses one lowercase DNS label. Anything that is not exactly
// one of the three forms, with a slug that passes ValidSlug, is rejected.
func ParseHostLabel(label string) (Host, bool) {
	slug, suffix, hasSuffix := strings.Cut(label, hostSep)
	if !ValidSlug(slug) {
		return Host{}, false
	}
	if !hasSuffix {
		return Host{slug: slug}, true
	}
	if suffix == "raw" {
		return Host{slug: slug, raw: true}, true
	}
	digits, ok := strings.CutPrefix(suffix, "v")
	if !ok || digits == "" || len(digits) > 6 || digits[0] == '0' {
		return Host{}, false
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return Host{}, false
		}
	}
	n, _ := strconv.Atoi(digits)
	return Host{slug: slug, raw: true, version: n}, true
}

// ViewerHost is the shared-link host of a valid slug.
func ViewerHost(slug string) Host { return Host{slug: slug} }

// RawHost is the raw host of a valid slug: version 0 for the current
// version's stable origin, N for the origin pinned to version N.
func RawHost(slug string, version int) Host {
	if version < 0 || version > MaxHostVersion {
		panic("ids: raw host version out of range")
	}
	return Host{slug: slug, raw: true, version: version}
}

// Label formats the host label. ParseHostLabel(h.Label()) returns h.
func (h Host) Label() string {
	switch {
	case !h.raw:
		return h.slug
	case h.version == 0:
		return h.slug + hostSep + "raw"
	}
	return h.slug + hostSep + "v" + strconv.Itoa(h.version)
}

func (h Host) Slug() string { return h.slug }
func (h Host) IsRaw() bool  { return h.raw }

// Version is the pinned version of a raw host; 0 for "--raw" and the viewer.
func (h Host) Version() int { return h.version }

// Viewer is the viewer host of the same artifact.
func (h Host) Viewer() Host { return Host{slug: h.slug} }
