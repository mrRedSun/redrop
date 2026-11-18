package httpapi

import (
	"context"
	"net/http"

	"github.com/coder/websocket"
	"github.com/go-fuego/fuego"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/internal/sharing"
	"github.com/mrRedSun/redrop/internal/tunnel"
	"github.com/mrRedSun/redrop/pkg/api"
)

// SetTunnels connects the API to the registry of live tunnel connections
// (DESIGN.md "Tunnels"). Without it, connecting a tunnel is refused.
func (a *API) SetTunnels(r *tunnel.Registry) { a.tunnels = r }

func (a *API) createTunnel(c fuego.ContextWithBody[api.CreateTunnelRequest]) (api.Artifact, error) {
	body, err := c.Body()
	if err != nil {
		return api.Artifact{}, err
	}
	m, err := a.sharing.CreateTunnel(c.Context(), actorFrom(c.Context()), sharing.TunnelSettings{
		Title: body.Title, TTL: body.TTL, Password: body.Password, InfoButton: body.InfoButton, Feedback: body.Feedback,
	})
	if err != nil {
		return api.Artifact{}, err
	}
	return artifactDTO(m), nil
}

// connectTunnel upgrades GET /api/v1/tunnels/{id}/connect to the tunnel's
// WebSocket. Every check runs before the upgrade and fails as a JSON
// problem; after it, the registry serves the connection until it ends and
// the WebSocket is closed with the reason.
func (a *API) connectTunnel(w http.ResponseWriter, r *http.Request) {
	if a.tunnels == nil {
		writeError(w, r, apperr.Forbidden("tunnels are turned off on this server"))
		return
	}
	owner, err := a.sharing.TunnelOwner(r.Context(), actorFrom(r.Context()), r.PathValue("id"), r.Header.Get(api.TunnelSessionHeader))
	if err != nil {
		writeError(w, r, err)
		return
	}
	lease, err := a.tunnels.Reserve(owner)
	if err != nil {
		writeError(w, r, err)
		return
	}
	// Accept refuses a browser's cross-origin handshake; a browser can't
	// send the Authorization header this route requires anyway.
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		lease.Cancel()
		return
	}
	conn := websocket.NetConn(context.Background(), ws, websocket.MessageBinary)
	lease.Serve(conn, func(code int, reason string) { ws.Close(websocket.StatusCode(code), reason) })
}

func (h adminAPI) listTunnels(c fuego.ContextNoBody) (api.TunnelList, error) {
	list, err := h.moderation.Tunnels(c.Context())
	if err != nil {
		return api.TunnelList{}, err
	}
	return api.TunnelList{Items: mapItems(list, tunnelDTO)}, nil
}

func (h adminAPI) endTunnel(c fuego.ContextNoBody) (api.OK, error) {
	err := h.moderation.EndTunnel(c.Context(), principalFrom(c.Context()).Username(), c.PathParam("id"))
	return api.OK{OK: err == nil}, err
}
