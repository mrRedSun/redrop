package config

import (
	"fmt"
	"math"
	"strconv"
	"time"
)

// Tunnels are the limits of `redrop tunnel` (DESIGN.md "Tunnels").
type Tunnels struct {
	// PerUser is how many tunnels one user may have connected at once;
	// 0 turns tunnels off.
	PerUser int
	// Streams is how many requests and WebSockets one tunnel may have open;
	// StreamsTotal bounds the same across the server (and with it the
	// memory tunnels can hold).
	Streams      int
	StreamsTotal int
	// MaxRequestBytes and MaxResponseBytes bound one body each way.
	MaxRequestBytes  int64
	MaxResponseBytes int64
	// BytesPerHour is each tunnel's traffic budget, both ways, per fixed
	// one-hour window.
	BytesPerHour int64
	// ResponseTimeout bounds a request's wait for a free stream slot, and
	// then the app's wait to send response headers once the request
	// (body included) is sent.
	ResponseTimeout time.Duration
	// IdleTimeout closes a request or WebSocket that moved no bytes either
	// way for this long.
	IdleTimeout time.Duration
}

// Enabled reports whether users may open tunnels.
func (t Tunnels) Enabled() bool { return t.PerUser > 0 }

func envNonNegativeInt(key, def string) (int, error) {
	n, err := strconv.Atoi(env(key, def))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a whole number, 0 or more (got %q)", key, env(key, def))
	}
	return n, nil
}

func envPositiveBytes(key, def string) (int64, error) {
	n, err := envBytes(key, def)
	if err != nil {
		return 0, err
	}
	if n < 1 {
		return 0, fmt.Errorf("%s must be at least 1 byte (got %q)", key, env(key, def))
	}
	return n, nil
}

func envPositiveDuration(key, def string) (time.Duration, error) {
	d, never, err := envDuration(key, def)
	if err != nil {
		return 0, err
	}
	if never {
		return 0, fmt.Errorf("%s must be a duration such as 90s, not %q", key, env(key, def))
	}
	return d, nil
}

func loadTunnels() (Tunnels, error) {
	var t Tunnels
	var err error
	if t.PerUser, err = envNonNegativeInt("REDROP_TUNNELS_PER_USER", "3"); err != nil {
		return t, err
	}
	if t.Streams, err = envPositiveInt("REDROP_TUNNEL_STREAMS", "32"); err != nil {
		return t, err
	}
	if t.StreamsTotal, err = envPositiveInt("REDROP_TUNNEL_STREAMS_TOTAL", "256"); err != nil {
		return t, err
	}
	if t.MaxRequestBytes, err = envPositiveBytes("REDROP_TUNNEL_MAX_REQUEST_SIZE", "100MB"); err != nil {
		return t, err
	}
	if t.MaxResponseBytes, err = envPositiveBytes("REDROP_TUNNEL_MAX_RESPONSE_SIZE", "1GB"); err != nil {
		return t, err
	}
	if t.BytesPerHour, err = envPositiveBytes("REDROP_TUNNEL_BYTES_PER_HOUR", "2GB"); err != nil {
		return t, err
	}
	// The budget is counted by security.Limiter, whose limit is an int.
	if t.BytesPerHour > math.MaxInt {
		return t, fmt.Errorf("REDROP_TUNNEL_BYTES_PER_HOUR is too large")
	}
	if t.ResponseTimeout, err = envPositiveDuration("REDROP_TUNNEL_RESPONSE_TIMEOUT", "90s"); err != nil {
		return t, err
	}
	if t.IdleTimeout, err = envPositiveDuration("REDROP_TUNNEL_IDLE_TIMEOUT", "100s"); err != nil {
		return t, err
	}
	return t, nil
}
