package sharing

import (
	"context"
	"errors"
	"time"

	"github.com/mrRedSun/redrop/internal/store"
	"github.com/mrRedSun/redrop/pkg/api"
)

// The viewer frame (DESIGN.md "Viewer frame"). A top-level page load of an
// HTML artifact or bundle with the info button on gets redrop's viewer page,
// which frames the artifact from the same origin. Every other request gets
// the stored bytes.

// Mode is how a version of an artifact is presented on its link.
type Mode int

const (
	ModeDirect   Mode = iota + 1 // stored bytes, as without the info button
	ModeFramed                   // top-level page loads get the viewer page
	ModeFilePage                 // redrop's own file page, with the info panel when it is on
)

// modeFor is the only place that decides which kinds get the frame.
func modeFor(kind string, infoOn bool) Mode {
	switch {
	case kind == api.KindFile:
		return ModeFilePage
	case infoOn:
		return ModeFramed
	}
	return ModeDirect
}

// Mode returns how snap, a version of a, is presented.
func (s *Service) Mode(a *store.Artifact, snap Snapshot) Mode {
	return modeFor(snap.Kind, s.infoButton(a))
}

// directWindow is how long a visitor's ?redrop-info=0 choice lasts.
const directWindow = 30 * time.Minute

// DirectChoice records that the visitor asked for the direct view of a
// (?redrop-info=0), so the pages it loads stay direct too.
func (s *Service) DirectChoice(a *store.Artifact) *Grant {
	return &Grant{Value: s.directSigner.Sign(grantPayload(a, "direct"), time.Now().Add(directWindow)), TTL: directWindow}
}

// Direct reports whether value is a current direct-view choice for a.
func (s *Service) Direct(a *store.Artifact, value string) bool {
	p, ok := s.directSigner.Verify(value)
	return ok && p == grantPayload(a, "direct")
}

// InfoShown reports whether a's pages carry the info button.
func (s *Service) InfoShown(a *store.Artifact) bool { return s.infoButton(a) }

// ErrVersionUnavailable means a requested older version may not be opened:
// the owner hasn't enabled it, the info button is off, or the version is not
// retained.
var ErrVersionUnavailable = errors.New("version unavailable")

// SelectVersion returns the version a viewer asked for: 0 or the current
// number means the current version.
func (s *Service) SelectVersion(ctx context.Context, a *store.Artifact, v int) (Snapshot, error) {
	if v == 0 || v == a.CurrentVersion {
		return CurrentSnapshot(a), nil
	}
	return s.OldVersion(ctx, a, v)
}

// OldVersion returns retained version v of a for the old-version path space,
// only when the owner lets viewers open older versions. The current version
// is served there too, so a frame keeps working across a rollback.
func (s *Service) OldVersion(ctx context.Context, a *store.Artifact, v int) (Snapshot, error) {
	if !s.oldVersionsOpen(a) || v <= 0 {
		return Snapshot{}, ErrVersionUnavailable
	}
	snap, ok := s.snapshotAt(ctx, a, v)
	if !ok {
		return Snapshot{}, ErrVersionUnavailable
	}
	return snap, nil
}

func (s *Service) oldVersionsOpen(a *store.Artifact) bool { return a.OldVersions && s.infoButton(a) }
