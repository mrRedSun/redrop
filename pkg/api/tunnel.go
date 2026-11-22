package api

import "time"

// KindTunnel is a live app shared from the owner's computer through
// `redrop tunnel`. It has no stored files: requests to its link are
// forwarded to the connected CLI.
const KindTunnel = "tunnel"

// TunnelSessionHeader carries a random value the CLI picks once per process.
// A connection with the same value replaces the previous one at once, so a
// reconnecting CLI is never locked out by its own dead connection; another
// process gets 409 while the tunnel is connected. It is not a secret.
const TunnelSessionHeader = "X-Redrop-Tunnel-Session"

// Problem titles of tunnel connection refusals.
const (
	// CodeTunnelInUse (409): another redrop tunnel process holds the tunnel.
	// It is temporary; the CLI keeps retrying.
	CodeTunnelInUse = "tunnel_in_use"
	// CodeTunnelLimit (429): the user has as many tunnels connected as the
	// server allows. The CLI stops.
	CodeTunnelLimit = "tunnel_limit"
	// CodeTunnelCount (409): the user keeps as many tunnels as the server
	// allows, so no new one can be created; reuse one (--id) or delete one.
	CodeTunnelCount = "tunnel_count_limit"
)

// WebSocket close codes the server ends a tunnel connection with. Any other
// end (a network failure, 1001 for a server restart) means reconnect.
const (
	// TunnelCloseEnded: the tunnel may not run any more: revoked, deleted,
	// expired, its owner disabled, its token deleted or expired, or ended by
	// an administrator. The close reason says which. The CLI exits.
	TunnelCloseEnded = 4001
	// TunnelCloseReplaced: a newer connection from the same CLI process took
	// over. The CLI reconnects.
	TunnelCloseReplaced = 4002
)

// CreateTunnelRequest creates a tunnel artifact. A view limit, older
// versions and the SPA flag don't apply to tunnels.
type CreateTunnelRequest struct {
	Title      string `json:"title,omitempty"`
	TTL        string `json:"ttl,omitempty" description:"Lifetime like 30m, 12h, 7d, 2w, or never; empty uses the server default"`
	Password   string `json:"password,omitempty"`
	InfoButton *bool  `json:"info_button,omitempty" description:"Show the info button and open pages in the viewer frame; omitted uses the server default"`
	Feedback   bool   `json:"feedback,omitempty" description:"Let viewers leave comments from the info panel (default false; needs the info button)"`
}

// Tunnel is a connected tunnel in the administrators' list: metadata only,
// never the link.
type Tunnel struct {
	ID            string    `json:"id" description:"Artifact ID"`
	Title         string    `json:"title"`
	OwnerUsername string    `json:"owner_username"`
	TokenName     string    `json:"token_name" description:"API token the CLI connected with"`
	ConnectedAt   time.Time `json:"connected_at"`
	Streams       int       `json:"streams" description:"Open requests and WebSockets"`
	BytesThisHour int64     `json:"bytes_this_hour" description:"Bytes both ways in the current hourly budget window"`
}

type TunnelList struct {
	Items []Tunnel `json:"items"`
}
