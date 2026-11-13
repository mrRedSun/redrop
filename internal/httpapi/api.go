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
	cfg      *config.Config
	clients  *netutil.ClientResolver
	log      *slog.Logger
	cookies  cookies
	server   *fuego.Server
}

func New(acc *accounts.Service, sh *sharing.Service, cfg *config.Config, clients *netutil.ClientResolver, log *slog.Logger) *API {
	a := &API{accounts: acc, sharing: sh, cfg: cfg, clients: clients, log: log, cookies: cookies{insecure: cfg.Insecure}}
	a.server = fuego.NewServer(
		fuego.WithoutStartupMessages(),
		fuego.WithoutLogger(),
		fuego.WithLogHandler(log.Handler()),
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
