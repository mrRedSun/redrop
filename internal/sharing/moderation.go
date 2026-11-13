package sharing

import (
	"context"
	"strings"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/internal/store"
)

// Moderation is the administrators' view of sharing. It acts on any user's
// artifact but only ever returns AdminArtifact, which cannot carry the secret
// link, and it never exposes content (DESIGN.md: "Admin privacy"). Admin
// transports hold a *Moderation instead of the owner *Service, so nothing an
// admin handler can reach returns a link.
type Moderation struct{ s *Service }

// Moderation returns the admin-facing port of the service.
func (s *Service) Moderation() *Moderation { return &Moderation{s: s} }

// List returns every artifact's metadata.
func (m *Moderation) List(ctx context.Context, query, status string) ([]AdminArtifact, error) {
	rows, err := m.s.list(ctx, store.ListFilter{Query: strings.TrimSpace(query)}, status)
	if err != nil {
		return nil, err
	}
	out := make([]AdminArtifact, 0, len(rows))
	for _, a := range rows {
		out = append(out, m.s.adminView(a))
	}
	return out, nil
}

// SetRevoked revokes or restores any artifact.
func (m *Moderation) SetRevoked(ctx context.Context, admin, id string, revoked bool) (AdminArtifact, error) {
	if err := m.s.db.UpdateArtifact(ctx, id, store.ArtifactUpdate{Revoked: &revoked}); err != nil {
		return AdminArtifact{}, mapStore(err)
	}
	a, err := m.s.db.ArtifactByID(ctx, id)
	if err != nil {
		return AdminArtifact{}, mapStore(err)
	}
	m.s.log.Info("admin changed artifact", "admin", admin, "id", id, "owner", a.OwnerUsername, "revoked", revoked)
	return m.s.adminView(a), nil
}

// Delete deletes any artifact and all its versions.
func (m *Moderation) Delete(ctx context.Context, admin, id string) error {
	if err := m.s.db.DeleteArtifact(ctx, id); err != nil {
		return mapStore(err)
	}
	m.s.log.Info("admin deleted artifact", "admin", admin, "id", id)
	return nil
}

// Stats summarizes server usage.
func (m *Moderation) Stats(ctx context.Context) (Stats, error) {
	st, err := m.s.db.Stats(ctx)
	if err != nil {
		return Stats{}, apperr.Internal(err)
	}
	stored, err := m.s.blobs.Usage()
	if err != nil {
		return Stats{}, apperr.Internal(err)
	}
	return Stats{Users: st.Users, Artifacts: st.Artifacts, Active: st.Active, LogicalBytes: st.LogicalBytes,
		StoredBytes: stored, MaxTotalBytes: m.s.cfg.MaxTotalBytes, MaxVersionBytes: m.s.cfg.MaxVersionBytes}, nil
}
