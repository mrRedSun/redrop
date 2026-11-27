package httpapi

import (
	"github.com/go-fuego/fuego"

	"github.com/mrRedSun/redrop/pkg/api"
)

// Live reload leases (DESIGN.md "Live reload"), held by `redrop watch`.

func (a *API) holdLive(c fuego.ContextNoBody) (api.LiveLease, error) {
	l, err := a.sharing.HoldLive(c.Context(), actorFrom(c.Context()), c.PathParam("id"), c.PathParam("session"))
	if err != nil {
		return api.LiveLease{}, err
	}
	return liveLeaseDTO(l), nil
}

func (a *API) releaseLive(c fuego.ContextNoBody) (api.OK, error) {
	err := a.sharing.ReleaseLive(c.Context(), actorFrom(c.Context()), c.PathParam("id"), c.PathParam("session"))
	return api.OK{OK: err == nil}, err
}
