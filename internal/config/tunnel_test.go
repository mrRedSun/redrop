package config

import (
	"strings"
	"testing"
	"time"
)

func TestTunnelLimits(t *testing.T) {
	t.Setenv("REDROP_PUBLIC_URL", "https://redrop.example.com")
	t.Setenv("REDROP_CONTENT_URL", "https://usercontent.example")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := Tunnels{PerUser: 3, Stored: 20, Streams: 32, StreamsTotal: 256, MaxRequestBytes: 100e6, MaxResponseBytes: 1e9,
		BytesPerHour: 2e9, ResponseTimeout: 90 * time.Second, IdleTimeout: 100 * time.Second}
	if c.Tunnels != want || !c.Tunnels.Enabled() {
		t.Fatalf("defaults: %+v, want %+v", c.Tunnels, want)
	}

	t.Setenv("REDROP_TUNNELS_PER_USER", "0")
	t.Setenv("REDROP_TUNNEL_MAX_RESPONSE_SIZE", "512MiB")
	t.Setenv("REDROP_TUNNEL_RESPONSE_TIMEOUT", "30s")
	if c, err = Load(); err != nil || c.Tunnels.Enabled() || c.Tunnels.MaxResponseBytes != 512<<20 || c.Tunnels.ResponseTimeout != 30*time.Second {
		t.Fatalf("overrides: %+v %v", c.Tunnels, err)
	}

	for key, bad := range map[string][]string{
		"REDROP_TUNNELS_PER_USER":          {"-1", "many"},
		"REDROP_TUNNEL_ARTIFACTS_PER_USER": {"0", "lots"},
		"REDROP_TUNNEL_STREAMS":            {"0", "x"},
		"REDROP_TUNNEL_STREAMS_TOTAL":      {"0"},
		"REDROP_TUNNEL_MAX_REQUEST_SIZE":   {"0", "big"},
		"REDROP_TUNNEL_MAX_RESPONSE_SIZE":  {"0B"},
		"REDROP_TUNNEL_BYTES_PER_HOUR":     {"0", "-5MB"},
		"REDROP_TUNNEL_RESPONSE_TIMEOUT":   {"never", "soon"},
		"REDROP_TUNNEL_IDLE_TIMEOUT":       {"0s", "never"},
	} {
		t.Run(key, func(t *testing.T) {
			for _, v := range bad {
				t.Setenv(key, v)
				if _, err := Load(); err == nil || !strings.Contains(err.Error(), key) {
					t.Fatalf("%s=%q: err %v, want an error naming the variable", key, v, err)
				}
			}
		})
	}
}
