package sharing

import (
	"context"
	"strings"
	"time"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/internal/store"
	"github.com/mrRedSun/redrop/internal/tunnel"
	"github.com/mrRedSun/redrop/pkg/api"
)

// Tunnels (DESIGN.md "Tunnels") are artifacts of kind tunnel: one version
// with no files, served live by the owner's `redrop tunnel` through the
// connection registry. This file holds their rules; everything else (links,
// gates, revocation, deletion) is the artifact code they share.

// defaultTunnelTitle names a tunnel created without a title.
const defaultTunnelTitle = "Tunnel"

// SetTunnels connects the service to the registry of live tunnel
// connections, so changes to a tunnel reach its connection and listings show
// whether it is online. Without it, tunnels are always offline.
func (s *Service) SetTunnels(r *tunnel.Registry) { s.tunnels = r }

// IsTunnel reports whether a is a tunnel.
func IsTunnel(a *store.Artifact) bool { return a.Kind == api.KindTunnel }

// TunnelSettings are the settings of a new tunnel.
type TunnelSettings struct {
	Title      string
	TTL        string // "" = server default, "never", or a duration
	Password   string
	InfoButton *bool // nil = server default
	Feedback   bool
}

// CreateTunnel creates a tunnel artifact. It stores no content, so it takes
// no upload and no quota.
func (s *Service) CreateTunnel(ctx context.Context, actor Actor, set TunnelSettings) (Artifact, error) {
	if !s.cfg.Tunnels.Enabled() {
		return Artifact{}, apperr.Forbidden("tunnels are turned off on this server")
	}
	exp, err := s.expiry(set.TTL)
	if err != nil {
		return Artifact{}, err
	}
	pwHash, err := hashArtifactPassword(set.Password)
	if err != nil {
		return Artifact{}, err
	}
	title := truncate(strings.TrimSpace(set.Title), maxTitle)
	if title == "" {
		title = defaultTunnelTitle
	}
	id, err := s.db.CreateTunnel(ctx, &store.NewArtifact{
		UserID: actor.UserID, TokenID: actor.TokenID, TokenName: actor.TokenName,
		Title: title, PasswordHash: pwHash, ExpiresAt: exp, InfoButton: set.InfoButton, Feedback: set.Feedback,
	})
	if err != nil {
		return Artifact{}, mapStore(err)
	}
	s.log.Info("tunnel created", "id", id, "user", actor.Username, "token", actor.TokenName)
	return s.reload(ctx, id)
}

// TunnelOwner checks that actor may connect tunnel id now and returns what
// the registry needs: the artifact must be the actor's tunnel, and its link
// live. client is the CLI's session value.
func (s *Service) TunnelOwner(ctx context.Context, actor Actor, id, client string) (tunnel.Owner, error) {
	if !s.cfg.Tunnels.Enabled() {
		return tunnel.Owner{}, apperr.Forbidden("tunnels are turned off on this server")
	}
	a, err := s.owned(ctx, actor, id)
	if err != nil {
		return tunnel.Owner{}, err
	}
	if !IsTunnel(a) {
		return tunnel.Owner{}, apperr.Invalid("artifact %s is a %s, not a tunnel; create one with redrop tunnel PORT", a.ID, a.Kind)
	}
	switch a.Status(time.Now()) {
	case api.StatusRevoked:
		if a.AdminRevoked {
			return tunnel.Owner{}, apperr.Gone("an administrator revoked this tunnel")
		}
		return tunnel.Owner{}, apperr.Gone("this tunnel is revoked; restore it with redrop restore %s", a.ID)
	case api.StatusExpired:
		return tunnel.Owner{}, apperr.Gone("this tunnel expired; extend it with redrop set %s --ttl 1d", a.ID)
	}
	return tunnel.Owner{
		ArtifactID: a.ID, UserID: actor.UserID, Username: actor.Username, TokenID: actor.TokenID, TokenName: actor.TokenName,
		TokenExpires: actor.TokenExpires, Expires: a.ExpiresAt, AccessGen: a.AccessGen, Client: client,
	}, nil
}

// tunnelPatch refuses settings that don't apply to a live app.
func tunnelPatch(p Patch) error {
	switch {
	case p.MaxViews != nil && *p.MaxViews != 0:
		return apperr.Invalid("a tunnel can't have a view limit")
	case p.SPA != nil:
		return apperr.Invalid("a tunnel has no single-page-app setting; the app answers every path")
	case p.OldVersions != nil && *p.OldVersions:
		return apperr.Invalid("a tunnel has no older versions")
	}
	return nil
}

// errTunnelVersions is the answer to publishing, activating or deleting a
// tunnel's version.
func errTunnelVersions() error {
	return apperr.Invalid("a tunnel has no versions to publish or change; redrop tunnel serves it live")
}

// syncTunnel tells the registry about a tunnel's access after a change:
// a link that is no longer live ends its connection; otherwise the expiry
// and access generation are updated.
func (s *Service) syncTunnel(ctx context.Context, id string) {
	if s.tunnels == nil {
		return
	}
	a, err := s.db.ArtifactByID(ctx, id)
	if err != nil {
		s.tunnels.End(id, tunnel.WhyDeleted)
		return
	}
	if !IsTunnel(a) {
		return
	}
	why := ""
	switch a.Status(time.Now()) {
	case api.StatusRevoked:
		why = tunnel.WhyRevoked
		if a.AdminRevoked {
			why = tunnel.WhyAdminRevoked
		}
	case api.StatusExpired:
		why = tunnel.WhyExpired
	}
	s.tunnels.Update(id, why == "", a.AccessGen, a.ExpiresAt, why)
}

// tunnelOnline reports whether a is a connected tunnel.
func (s *Service) tunnelOnline(a *store.Artifact) bool {
	return s.tunnels != nil && IsTunnel(a) && s.tunnels.Online(a.ID)
}

// AdminTunnel is a connected tunnel in the administrators' list: metadata
// only, never the link.
type AdminTunnel struct {
	ID            string
	Title         string
	OwnerUsername string
	TokenName     string
	ConnectedAt   time.Time
	Streams       int
	BytesThisHour int64
}

// Tunnels lists the connected tunnels.
func (m *Moderation) Tunnels(ctx context.Context) ([]AdminTunnel, error) {
	out := []AdminTunnel{}
	if m.s.tunnels == nil {
		return out, nil
	}
	for _, t := range m.s.tunnels.Connected() {
		row := AdminTunnel{ID: t.ArtifactID, OwnerUsername: t.Username, TokenName: t.TokenName, ConnectedAt: t.ConnectedAt, Streams: t.Streams, BytesThisHour: t.BytesThisHour}
		if a, err := m.s.db.ArtifactByID(ctx, t.ArtifactID); err == nil {
			row.Title, row.OwnerUsername = a.Title, a.OwnerUsername
		}
		out = append(out, row)
	}
	return out, nil
}

// EndTunnel ends a tunnel's connection. The CLI exits; the owner may start
// it again. To keep it off, revoke it.
func (m *Moderation) EndTunnel(ctx context.Context, admin, id string) error {
	if m.s.tunnels == nil || !m.s.tunnels.End(id, tunnel.WhyAdmin) {
		return apperr.NotFound("no tunnel with this ID is connected")
	}
	m.s.log.Info("admin ended tunnel", "admin", admin, "id", id)
	return nil
}
