// Package httpapi is the JSON API transport (Fuego). Handlers only decode
// input, call the accounts/sharing services, and map results to wire types.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-fuego/fuego"
	"github.com/go-fuego/fuego/option"

	"github.com/mrRedSun/redrop/internal/accounts"
	"github.com/mrRedSun/redrop/internal/config"
	"github.com/mrRedSun/redrop/internal/netutil"
	"github.com/mrRedSun/redrop/internal/sharing"
)

// Version is reported by /api/v1/auth/config; set at build time.
var Version = "dev"

type API struct {
	accounts *accounts.Service
	sharing  *sharing.Service
	admin    adminAPI
	cfg      *config.Config
	clients  *netutil.ClientResolver
	log      *slog.Logger
	cookies  cookies
	server   *fuego.Server
}

func New(acc *accounts.Service, sh *sharing.Service, cfg *config.Config, clients *netutil.ClientResolver, log *slog.Logger) *API {
	a := &API{accounts: acc, sharing: sh, admin: adminAPI{users: acc.Administration(), moderation: sh.Moderation()}, cfg: cfg, clients: clients, log: log, cookies: cookies{insecure: cfg.Insecure}}
	a.server = fuego.NewServer(
		fuego.WithoutStartupMessages(),
		// Fuego's access log writes raw paths, which can hold one-time
		// secrets (invite and reset tokens). Requests are logged only by
		// logRequests, which redacts them.
		fuego.WithLoggingMiddleware(fuego.LoggingConfig{DisableRequest: true, DisableResponse: true}),
		fuego.WithLogHandler(levelFloor{Handler: log.Handler(), min: slog.LevelInfo}),
		fuego.WithMaxBodySize(1<<20),
		fuego.WithSecurity(openapi3.SecuritySchemes{
			"bearer": &openapi3.SecuritySchemeRef{Value: openapi3.NewSecurityScheme().
				WithType("http").WithScheme("bearer").WithDescription("API token (rdp_…)")},
			"session": &openapi3.SecuritySchemeRef{Value: openapi3.NewSecurityScheme().
				WithType("apiKey").WithIn("cookie").WithName(a.cookies.sessionName()).WithDescription("Dashboard session")},
		}),
		fuego.WithEngineOptions(
			fuego.WithErrorHandler(func(ctx context.Context, err error) error { return toProblem(ctx, err) }),
			fuego.WithOpenAPIConfig(fuego.OpenAPIConfig{
				DisableLocalSave:     true,
				DisableMessages:      true,
				DisableDefaultServer: true,
				PrettyFormatJSON:     true,
				SpecURL:              "/api/openapi.json",
				SwaggerURL:           "/api/docs",
				DisableSwaggerUI:     !cfg.APIDocs,
				Info: &openapi3.Info{
					Title:       "redrop API",
					Description: "Share agent artifacts through expiring, optionally protected links.",
					Version:     Version,
				},
			}),
		),
	)
	a.routes()
	a.server.RegisterOpenAPIRoutes(a.server)
	return a
}

// levelFloor drops records below min. Fuego logs through the default slog
// logger, which New points at redrop's handler; its debug messages include
// decoded request bodies, with passwords and one-time tokens, so they must
// never be written whatever level redrop's logger runs at.
type levelFloor struct {
	slog.Handler
	min slog.Level
}

func (h levelFloor) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= h.min && h.Handler.Enabled(ctx, l)
}

func (h levelFloor) WithAttrs(a []slog.Attr) slog.Handler {
	return levelFloor{Handler: h.Handler.WithAttrs(a), min: h.min}
}

func (h levelFloor) WithGroup(name string) slog.Handler {
	return levelFloor{Handler: h.Handler.WithGroup(name), min: h.min}
}

// Mount registers extra non-API handlers (dashboard, installers) on the app origin.
func (a *API) Mount(pattern string, h http.HandlerFunc) {
	fuego.GetStd(a.server, pattern, h, option.Hide())
}

// Handler is the app-origin handler with security headers and logging.
func (a *API) Handler() http.Handler {
	return a.logRequests(a.securityHeaders(a.server.Mux))
}

// OpenAPI returns the generated OpenAPI document.
func (a *API) OpenAPI() ([]byte, error) {
	return a.server.OutputOpenAPISpec().MarshalJSON()
}
