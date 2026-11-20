package sharing

import (
	"context"
	"strings"
	"time"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/internal/e2e"
	"github.com/mrRedSun/redrop/internal/ids"
	"github.com/mrRedSun/redrop/internal/store"
	"github.com/mrRedSun/redrop/pkg/api"
)

// End-to-end encrypted artifacts (DESIGN.md "End-to-end encrypted links").
// The server never has the key. It stores ciphertext objects, checks their
// layout and magic, keeps every version of an artifact on the same side of
// the line, and compares each version's key check with the artifact's.

// E2EContent is what a publication of end-to-end encrypted content declares.
type E2EContent struct {
	// KeyCheck identifies the key the content was encrypted with.
	KeyCheck string
	// Version is the number the content was encrypted for.
	Version int
	// Reservation is the signed artifact ID reservation; new artifacts only.
	Reservation string
}

// reservationTTL is how long a reserved artifact ID can be used: longer
// than any upload.
const reservationTTL = 24 * time.Hour

// Reservation is an artifact ID minted ahead of the artifact, so content
// bound to it can be encrypted before upload.
type Reservation struct {
	ID        string
	Token     string // signed for the actor; passed back on create
	ExpiresAt time.Time
}

// Reserve mints an artifact ID for the actor's next end-to-end share.
func (s *Service) Reserve(actor Actor) Reservation {
	id := ids.ID()
	exp := time.Now().Add(reservationTTL)
	return Reservation{ID: id, Token: s.reservationSigner.Sign(reservationPayload(actor.UserID, id), exp), ExpiresAt: exp.UTC()}
}

func reservationPayload(userID, id string) string { return userID + "|" + id }

// reservedID returns the artifact ID a reservation token holds for the
// actor, or an error for a forged, expired or someone else's token.
func (s *Service) reservedID(actor Actor, token string) (string, error) {
	p, ok := s.reservationSigner.Verify(token)
	user, id, cut := strings.Cut(p, "|")
	if !ok || !cut || user != actor.UserID || id == "" {
		return "", apperr.Invalid("the artifact ID reservation is invalid or expired; publish again")
	}
	return id, nil
}

// checkE2EContent validates what an end-to-end publication declares.
func checkE2EContent(c Content) error {
	switch {
	case !e2e.ValidKeyCheck(c.E2E.KeyCheck):
		return apperr.Invalid("e2e.key_check is malformed")
	case c.E2E.Version < 1:
		return apperr.Invalid("e2e.version must be the version number the content was encrypted for")
	case c.SPA:
		return errE2ESPA
	}
	return nil
}

var errE2ESPA = apperr.Invalid("SPA fallback is not available for end-to-end encrypted artifacts")

// checkE2EVersion keeps every version of a on one side: a plaintext version
// can't join an end-to-end artifact or the reverse, and an end-to-end
// version must use the artifact's key.
func checkE2EVersion(a *store.Artifact, c Content) error {
	switch {
	case a.E2E && c.E2E == nil:
		return apperr.Invalid("this artifact is end-to-end encrypted; publish new versions with --e2e (redrop update --e2e, redrop watch --e2e)")
	case !a.E2E && c.E2E != nil:
		return apperr.Invalid("this artifact is not end-to-end encrypted; publish its new version without encryption")
	case c.E2E != nil && c.E2E.KeyCheck != a.E2EKeyCheck:
		return apperr.Invalid("the content was encrypted with a different key than this artifact's; use the artifact's link or its saved key")
	}
	return nil
}

// e2eFiles checks a materialized end-to-end version's layout: exactly one
// manifest, every other file a stored object name. extractBundle has
// checked that every file starts with the format's magic.
func e2eFiles(files []store.File) error {
	manifests := 0
	for _, f := range files {
		switch {
		case f.Path == e2e.ManifestPath:
			manifests++
		case !e2e.ValidObjectName(f.Path):
			return apperr.Invalid("an end-to-end encrypted version holds only its manifest and encrypted objects; found %q", f.Path)
		}
	}
	if manifests != 1 || len(files) < 2 {
		return apperr.Invalid("an end-to-end encrypted version needs its manifest and at least one file")
	}
	return nil
}

// E2EObject returns a stored ciphertext object of snap by its stored name
// (the manifest or an object name), or nil.
func (s *Service) E2EObject(ctx context.Context, snap Snapshot, name string) *store.File {
	if !snap.E2E || (name != e2e.ManifestPath && !e2e.ValidObjectName(name)) {
		return nil
	}
	return s.fileAt(ctx, snap, name)
}

// e2eContentType is how the server stores and serves ciphertext.
const e2eContentType = "application/octet-stream"

// codeVersionConflict is api.CodeVersionConflict.
const codeVersionConflict = api.CodeVersionConflict
