package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/mrRedSun/redrop/pkg/api"
)

// runTunnelCmd runs `redrop tunnel` against srv and returns the exit code
// and output.
func runTunnelCmd(t *testing.T, srv *httptest.Server, args ...string) (int, string, string) {
	t.Helper()
	code := exitOK
	out, errOut := captureCLIOutput(t, func() {
		cmd := newRootCmd()
		cmd.SetArgs(append([]string{"--url", srv.URL, "--token", "rdp_test", "tunnel"}, args...))
		if err := cmd.Execute(); err != nil {
			code = exitGeneral
			var ee *exitError
			if errors.As(err, &ee) {
				code = ee.code
			}
			t.Log(err)
		}
	})
	return code, out, errOut
}

func TestTunnelUsageErrorsNeedNoServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("contacted the server: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	for _, args := range [][]string{
		{"3000", "--once"},
		{"3000", "--max-views", "2"},
		{"3000", "--info", "--no-info"},
		{"3000", "--feedback", "--no-feedback"},
		{"3000", "--password", "a", "--password-generate"},
		{"https://localhost:3000"},
		{"192.0.2.10:8080"},
		{"0.0.0.0:3000", "--allow-remote-target"},
		{"not a target"},
	} {
		if code, _, _ := runTunnelCmd(t, srv, args...); code != exitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, exitUsage)
		}
	}
}

// fakeTunnelAPI answers creating or reading a tunnel, then the connect
// WebSocket with close (a code) or a problem status.
func fakeTunnelAPI(t *testing.T, kind string, connectStatus int, closeCode websocket.StatusCode) *httptest.Server {
	t.Helper()
	art := api.Artifact{ID: "tid", URL: "https://amber-otter-quilt-river.usercontent.example/", Kind: kind, Title: "localhost:3000", Status: api.StatusActive}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v1/tunnels":
			var req api.CreateTunnelRequest
			json.NewDecoder(r.Body).Decode(&req)
			if req.Title != "localhost:3000" || req.Password == "" {
				t.Errorf("create request %+v: want the target as title and the generated password", req)
			}
			art.HasPassword = true
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(art)
		case r.Method == "GET" && r.URL.Path == "/api/v1/artifacts/tid":
			json.NewEncoder(w).Encode(art)
		case r.URL.Path == "/api/v1/tunnels/tid/connect":
			if connectStatus != 0 {
				w.WriteHeader(connectStatus)
				json.NewEncoder(w).Encode(map[string]any{"title": "Gone", "detail": "this tunnel is revoked", "status": connectStatus})
				return
			}
			c, err := websocket.Accept(w, r, nil)
			if err == nil {
				c.Close(closeCode, "ended by an administrator")
			}
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTunnelPrintsTheLinkAndExitsWhenEnded(t *testing.T) {
	srv := fakeTunnelAPI(t, api.KindTunnel, 0, api.TunnelCloseEnded)
	code, out, _ := runTunnelCmd(t, srv, "3000", "--password-generate")
	if code != exitGone {
		t.Fatalf("exit %d, want %d after the server ended the tunnel", code, exitGone)
	}
	for _, want := range []string{"url:", "https://amber-otter-quilt-river.usercontent.example/", "password:", "target:", "http://localhost:3000 (127.0.0.1, ::1)", "access:", "password"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestTunnelJSONOutput(t *testing.T) {
	srv := fakeTunnelAPI(t, api.KindTunnel, 0, api.TunnelCloseEnded)
	code, out, _ := runTunnelCmd(t, srv, "3000", "--password-generate", "--json")
	if code != exitGone {
		t.Fatalf("exit %d", code)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var first, second tunnelEvent
	if len(lines) != 2 || json.Unmarshal([]byte(lines[0]), &first) != nil || json.Unmarshal([]byte(lines[1]), &second) != nil {
		t.Fatalf("output:\n%s", out)
	}
	if first.Event != "tunnel" || first.ID != "tid" || first.URL == "" || first.Password == "" || first.Target != "http://localhost:3000" || second.Event != "online" {
		t.Fatalf("events: %+v %+v", first, second)
	}
}

func TestTunnelReuseChecksTheKind(t *testing.T) {
	srv := fakeTunnelAPI(t, api.KindHTML, 0, 0)
	if code, _, _ := runTunnelCmd(t, srv, "3000", "--id", "tid"); code != exitUsage {
		t.Fatalf("--id of an HTML artifact: exit %d", code)
	}
	srv = fakeTunnelAPI(t, api.KindTunnel, http.StatusGone, 0)
	if code, _, _ := runTunnelCmd(t, srv, "3000", "--id", "tid"); code != exitGone {
		t.Fatalf("--id of a revoked tunnel: exit %d", code)
	}
}
