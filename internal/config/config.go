// Package config loads redrop server configuration from the environment.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mrRedSun/redrop/pkg/api"
)

type OIDCProvider struct {
	ID           string
	Label        string
	Issuer       string
	ClientID     string
	ClientSecret string
	Scopes       []string
}

type Config struct {
	Listen     string
	DataDir    string
	PublicURL  *url.URL // dashboard/API origin
	ContentURL *url.URL // content base; artifacts live at <slug>.<host>

	// Insecure disables Secure cookies and the __Host- prefix (plain HTTP dev only).
	Insecure bool

	DefaultTTL        time.Duration
	DefaultNever      bool
	MaxTTL            time.Duration // 0 = unlimited, never allowed
	AllowNever        bool
	ExpiredRetention  time.Duration
	MaxVersionBytes   int64
	MaxTotalBytes     int64
	DefaultQuotaBytes int64
	MaxFiles          int
	ChunkSize         int64
	KeepVersions      int // 0 = unlimited

	// InfoButtonDefault shows the viewer info button on HTML artifacts that
	// have no explicit setting of their own.
	InfoButtonDefault bool

	ViewWindowLimited  time.Duration
	ViewWindowPassword time.Duration

	// Artifact-host misses (unknown, deleted, or unavailable slugs) allowed
	// per client network: a burst, then a steady refill per minute.
	ContentMissBurst     int
	ContentMissPerMinute int

	TrustedProxies []*net.IPNet
	ClientIPHeader string // honoured only from trusted proxies
	CountryHeader  string

	OIDC             []OIDCProvider
	PasswordLogin    bool
	AdminRequiresSSO bool

	SecretFile string
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func envBool(key string, def bool) (bool, error) {
	v := env(key, "")
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s: %w", key, err)
	}
	return b, nil
}

// ParseBytes parses sizes such as 500MB, 50GB, 10GiB, 1048576.
func ParseBytes(s string) (int64, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	mult := int64(1)
	for _, u := range []struct {
		suf string
		m   int64
	}{
		{"TIB", 1 << 40}, {"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10},
		{"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3},
		{"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}, {"B", 1},
	} {
		if strings.HasSuffix(s, u.suf) {
			mult = u.m
			s = strings.TrimSpace(strings.TrimSuffix(s, u.suf))
			break
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return int64(f * float64(mult)), nil
}

func envBytes(key, def string) (int64, error) {
	n, err := ParseBytes(env(key, def))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

func envPositiveInt(key, def string) (int, error) {
	n, err := strconv.Atoi(env(key, def))
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s must be a positive integer (got %q)", key, env(key, def))
	}
	return n, nil
}

func envDuration(key, def string) (time.Duration, bool, error) {
	d, never, err := api.ParseTTL(env(key, def))
	if err != nil {
		return 0, false, fmt.Errorf("%s: %w", key, err)
	}
	return d, never, nil
}

func readSecret(direct, file string) (string, error) {
	if v := env(file, ""); v != "" {
		b, err := os.ReadFile(v)
		if err != nil {
			return "", fmt.Errorf("%s: %w", file, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return env(direct, ""), nil
}

func parseOrigin(key, def string) (*url.URL, error) {
	raw := env(key, def)
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("%s must be an origin like https://example.com (got %q)", key, raw)
	}
	u.Path = ""
	u.Host = strings.ToLower(u.Host)
	return u, nil
}

// Load reads configuration from REDROP_* environment variables.
func Load() (*Config, error) {
	c := &Config{
		Listen:     env("REDROP_LISTEN", ":8080"),
		DataDir:    env("REDROP_DATA_DIR", "/data"),
		SecretFile: env("REDROP_SECRET_FILE", ""),
	}
	var err error
	if c.PublicURL, err = parseOrigin("REDROP_PUBLIC_URL", "http://localhost:8080"); err != nil {
		return nil, err
	}
	if c.ContentURL, err = parseOrigin("REDROP_CONTENT_URL", ""); err != nil {
		return nil, err
	}
	if c.PublicURL.Hostname() == c.ContentURL.Hostname() || strings.HasSuffix(c.PublicURL.Hostname(), "."+c.ContentURL.Hostname()) {
		return nil, fmt.Errorf("REDROP_PUBLIC_URL must not be the content domain or one of its subdomains")
	}
	if c.Insecure, err = envBool("REDROP_INSECURE_COOKIES", c.PublicURL.Scheme == "http"); err != nil {
		return nil, err
	}

	if c.DefaultTTL, c.DefaultNever, err = envDuration("REDROP_DEFAULT_TTL", "7d"); err != nil {
		return nil, err
	}
	var maxNever bool
	if c.MaxTTL, maxNever, err = envDuration("REDROP_MAX_TTL", "never"); err != nil {
		return nil, err
	}
	if maxNever {
		c.MaxTTL = 0
	}
	if c.AllowNever, err = envBool("REDROP_ALLOW_NEVER", maxNever); err != nil {
		return nil, err
	}
	if c.ExpiredRetention, _, err = envDuration("REDROP_EXPIRED_RETENTION", "72h"); err != nil {
		return nil, err
	}
	if c.ViewWindowLimited, _, err = envDuration("REDROP_VIEW_WINDOW_LIMITED", "1h"); err != nil {
		return nil, err
	}
	if c.ViewWindowPassword, _, err = envDuration("REDROP_VIEW_WINDOW_PASSWORD", "12h"); err != nil {
		return nil, err
	}
	if c.MaxVersionBytes, err = envBytes("REDROP_MAX_VERSION_SIZE", "500MB"); err != nil {
		return nil, err
	}
	if c.MaxTotalBytes, err = envBytes("REDROP_MAX_TOTAL_SIZE", "50GB"); err != nil {
		return nil, err
	}
	if c.DefaultQuotaBytes, err = envBytes("REDROP_DEFAULT_QUOTA", "10GB"); err != nil {
		return nil, err
	}
	if c.ChunkSize, err = envBytes("REDROP_CHUNK_SIZE", "32MiB"); err != nil {
		return nil, err
	}
	if c.InfoButtonDefault, err = envBool("REDROP_INFO_BUTTON_DEFAULT", true); err != nil {
		return nil, err
	}
	if c.MaxFiles, err = strconv.Atoi(env("REDROP_MAX_FILES", "20000")); err != nil {
		return nil, fmt.Errorf("REDROP_MAX_FILES: %w", err)
	}
	if c.KeepVersions, err = strconv.Atoi(env("REDROP_KEEP_VERSIONS", "0")); err != nil {
		return nil, fmt.Errorf("REDROP_KEEP_VERSIONS: %w", err)
	}
	if c.ContentMissBurst, err = envPositiveInt("REDROP_CONTENT_MISS_BURST", "30"); err != nil {
		return nil, err
	}
	if c.ContentMissPerMinute, err = envPositiveInt("REDROP_CONTENT_MISS_PER_MINUTE", "5"); err != nil {
		return nil, err
	}

	for _, cidr := range strings.FieldsFunc(env("REDROP_TRUSTED_PROXIES", "127.0.0.1/32,::1/128"), func(r rune) bool { return r == ',' || r == ' ' }) {
		if !strings.Contains(cidr, "/") {
			if strings.Contains(cidr, ":") {
				cidr += "/128"
			} else {
				cidr += "/32"
			}
		}
		_, n, perr := net.ParseCIDR(cidr)
		if perr != nil {
			return nil, fmt.Errorf("REDROP_TRUSTED_PROXIES: %w", perr)
		}
		c.TrustedProxies = append(c.TrustedProxies, n)
	}
	c.ClientIPHeader = env("REDROP_CLIENT_IP_HEADER", "X-Forwarded-For")
	c.CountryHeader = env("REDROP_COUNTRY_HEADER", "CF-IPCountry")

	if c.PasswordLogin, err = envBool("REDROP_PASSWORD_LOGIN", true); err != nil {
		return nil, err
	}
	for _, id := range strings.FieldsFunc(env("REDROP_OIDC_PROVIDERS", ""), func(r rune) bool { return r == ',' || r == ' ' }) {
		id = strings.ToLower(id)
		up := "REDROP_OIDC_" + strings.ToUpper(strings.ReplaceAll(id, "-", "_")) + "_"
		secret, serr := readSecret(up+"CLIENT_SECRET", up+"CLIENT_SECRET_FILE")
		if serr != nil {
			return nil, serr
		}
		p := OIDCProvider{
			ID:           id,
			Label:        env(up+"LABEL", id),
			Issuer:       env(up+"ISSUER", ""),
			ClientID:     env(up+"CLIENT_ID", ""),
			ClientSecret: secret,
			Scopes:       strings.Fields(env(up+"SCOPES", "openid profile email")),
		}
		if p.Issuer == "" || p.ClientID == "" {
			return nil, fmt.Errorf("OIDC provider %q needs %sISSUER and %sCLIENT_ID", id, up, up)
		}
		c.OIDC = append(c.OIDC, p)
	}
	if c.AdminRequiresSSO, err = envBool("REDROP_ADMIN_REQUIRE_OIDC", false); err != nil {
		return nil, err
	}
	if c.AdminRequiresSSO && len(c.OIDC) == 0 {
		return nil, fmt.Errorf("REDROP_ADMIN_REQUIRE_OIDC needs at least one OIDC provider")
	}
	if !c.PasswordLogin && len(c.OIDC) == 0 {
		return nil, fmt.Errorf("REDROP_PASSWORD_LOGIN=false needs at least one OIDC provider")
	}
	return c, nil
}

// ContentHost returns the bare content domain (no port).
func (c *Config) ContentHost() string { return c.ContentURL.Hostname() }

// ArtifactURL builds https://<slug>.<content-domain>[:port]/.
func (c *Config) ArtifactURL(slug string) string {
	host := slug + "." + c.ContentURL.Hostname()
	if p := c.ContentURL.Port(); p != "" {
		host += ":" + p
	}
	return c.ContentURL.Scheme + "://" + host + "/"
}

// ContentFrameSource is the CSP source matching every artifact origin.
func (c *Config) ContentFrameSource() string {
	host := "*." + c.ContentURL.Hostname()
	if p := c.ContentURL.Port(); p != "" {
		host += ":" + p
	}
	return c.ContentURL.Scheme + "://" + host
}

// PublicOrigin returns scheme://host[:port] of the dashboard.
func (c *Config) PublicOrigin() string { return c.PublicURL.Scheme + "://" + c.PublicURL.Host }
