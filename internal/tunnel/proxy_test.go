package tunnel

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMultiplexesConcurrentRequests: many requests share one connection at
// once, each on its own stream, each getting its own answer.
func TestMultiplexesConcurrentRequests(t *testing.T) {
	const n = 32
	var arrived sync.WaitGroup
	arrived.Add(n)
	all := make(chan struct{})
	go func() { arrived.Wait(); close(all) }()
	lim := testLimits()
	lim.Streams = n
	f := newFixture(t, lim, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived.Done()
		// Every request is in flight before any answers: they really run
		// at the same time over the one connection.
		select {
		case <-all:
		case <-time.After(10 * time.Second):
		}
		io.WriteString(w, "answer "+r.URL.Path)
	}))
	f.connectWS(owner("t1", "u1"))
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			path := fmt.Sprintf("/r/%d", i)
			r, err := f.request(context.Background(), "GET", "t1", path, nil)
			if err != nil {
				errs <- err
				return
			}
			defer r.Body.Close()
			b, _ := io.ReadAll(r.Body)
			if r.StatusCode != 200 || string(b) != "answer "+path {
				errs <- fmt.Errorf("%s: %d %q", path, r.StatusCode, b)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestVisitorCancellationReachesTheApp: a visitor who goes away cancels the
// request at the local app.
func TestVisitorCancellationReachesTheApp(t *testing.T) {
	entered, cancelled := make(chan struct{}), make(chan struct{})
	f := newFixture(t, testLimits(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		close(entered)
		<-r.Context().Done()
		close(cancelled)
	}))
	f.connect(owner("t1", "u1"))
	ctx, cancel := context.WithCancel(context.Background())
	r, err := f.request(ctx, "GET", "t1", "/slow", nil)
	if err != nil {
		t.Fatal(err)
	}
	within(t, "the app to get the request", entered)
	cancel()
	r.Body.Close()
	within(t, "the app's request to be cancelled", cancelled)
	// The stream is gone on the server too.
	waitFor(t, "the stream to close", func() bool { _, n, _ := f.reg.connected("t1").state(); return n == 0 })
}

func TestOfflineWithoutAConnection(t *testing.T) {
	f := newFixture(t, testLimits(), http.NotFoundHandler())
	if status, body := f.get("t1", "/"); status != 503 || !strings.Contains(body, "This tunnel is offline") {
		t.Fatalf("not connected: %d %q", status, body)
	}
	p := f.connect(owner("t1", "u1"))
	if status, _ := f.get("t1", "/"); status != 404 {
		t.Fatalf("connected: %d, want the app's 404", status)
	}
	p.stop()
	waitFor(t, "the tunnel to go offline", func() bool { return !f.reg.Online("t1") })
	if status, _ := f.get("t1", "/"); status != 503 {
		t.Fatalf("after the CLI left: %d", status)
	}
}

func TestRequestSizeLimit(t *testing.T) {
	var reached int
	var mu sync.Mutex
	f := newFixture(t, testLimits(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reached++
		mu.Unlock()
		io.Copy(io.Discard, r.Body)
	}))
	f.connect(owner("t1", "u1"))
	big := bytes.Repeat([]byte("x"), 1<<20+1)
	// Declared: refused before anything is sent.
	r, err := f.request(context.Background(), "POST", "t1", "/upload", bytes.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusRequestEntityTooLarge || reached != 0 {
		t.Fatalf("declared too large: %d, app reached %d times", r.StatusCode, reached)
	}
	// Chunked: cut off at the limit.
	r, err = f.request(context.Background(), "POST", "t1", "/upload", io.MultiReader(bytes.NewReader(big)))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked too large: %d", r.StatusCode)
	}
	// At the limit it passes.
	r, err = f.request(context.Background(), "POST", "t1", "/upload", io.MultiReader(bytes.NewReader(big[:1<<20])))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("at the limit: %d", r.StatusCode)
	}
}

func TestResponseSizeLimit(t *testing.T) {
	f := newFixture(t, testLimits(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/declared" {
			w.Header().Set("Content-Length", fmt.Sprint(1<<20+1))
		}
		w.Write(bytes.Repeat([]byte("y"), 1<<20+1))
	}))
	f.connect(owner("t1", "u1"))
	if status, body := f.get("t1", "/declared"); status != http.StatusBadGateway || !strings.Contains(body, "larger than the server allows") {
		t.Fatalf("declared too large: %d %q", status, body)
	}
	r, err := f.request(context.Background(), "GET", "t1", "/chunked", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	n, err := io.Copy(io.Discard, r.Body)
	if err == nil || n > 1<<20 {
		t.Fatalf("chunked too large: read %d bytes, err %v; want it cut off at the limit", n, err)
	}
}

func TestStreamSlots(t *testing.T) {
	lim := testLimits()
	lim.Streams = 2
	release := make(chan struct{})
	entered := make(chan struct{}, 8)
	f := newFixture(t, lim, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
	}))
	f.connect(owner("t1", "u1"))
	type result struct{ status int }
	results := make(chan result, 4)
	send := func() {
		r, err := f.request(context.Background(), "GET", "t1", "/", nil)
		if err != nil {
			results <- result{-1}
			return
		}
		r.Body.Close()
		results <- result{r.StatusCode}
	}
	// Two run, two wait for a slot.
	for range 2 {
		go send()
	}
	within(t, "the first request", entered)
	within(t, "the second request", entered)
	for range 2 {
		go send()
	}
	waitFor(t, "two waiting requests", func() bool {
		l := f.reg.connected("t1")
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.waiting == 2
	})
	// A fifth is refused at once.
	if status, body := f.get("t1", "/"); status != 503 || !strings.Contains(body, "busy") {
		t.Fatalf("over the queue: %d %q", status, body)
	}
	close(release)
	for range 4 {
		if r := within(t, "a result", results); r.status != 200 {
			t.Fatalf("queued request: %d", r.status)
		}
	}
}

func TestSlotWaitTimesOut(t *testing.T) {
	lim := testLimits()
	lim.Streams, lim.ResponseTimeout = 1, 100*time.Millisecond
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	f := newFixture(t, lim, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		entered <- struct{}{}
		<-release
	}))
	defer close(release)
	f.connect(owner("t1", "u1"))
	r, err := f.request(context.Background(), "GET", "t1", "/", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	within(t, "the first request", entered)
	if status, body := f.get("t1", "/"); status != 503 || !strings.Contains(body, "busy") {
		t.Fatalf("no slot within the response timeout: %d %q", status, body)
	}
}

func TestServerWideStreamSlots(t *testing.T) {
	lim := testLimits()
	lim.StreamsTotal, lim.ResponseTimeout = 1, 100*time.Millisecond
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	f := newFixture(t, lim, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		entered <- struct{}{}
		<-release
	}))
	defer close(release)
	f.connect(owner("t1", "u1"))
	f.connect(owner("t2", "u2"))
	r, err := f.request(context.Background(), "GET", "t1", "/", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	within(t, "the first request", entered)
	if status, _ := f.get("t2", "/"); status != 503 {
		t.Fatalf("another tunnel past the server-wide limit: %d", status)
	}
}

func TestResponseTimeout(t *testing.T) {
	lim := testLimits()
	lim.ResponseTimeout = 100 * time.Millisecond
	release := make(chan struct{})
	f := newFixture(t, lim, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer close(release)
	f.connect(owner("t1", "u1"))
	if status, body := f.get("t1", "/"); status != http.StatusGatewayTimeout || !strings.Contains(body, "too long") {
		t.Fatalf("no answer in time: %d %q", status, body)
	}
}

func TestIdleTimeout(t *testing.T) {
	lim := testLimits()
	lim.IdleTimeout = 100 * time.Millisecond
	cancelled := make(chan struct{})
	f := newFixture(t, lim, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "start")
		w.(http.Flusher).Flush()
		<-r.Context().Done() // then nothing moves
		close(cancelled)
	}))
	f.connect(owner("t1", "u1"))
	r, err := f.request(context.Background(), "GET", "t1", "/", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if string(b) != "start" || err == nil {
		t.Fatalf("idle stream: read %q, err %v; want it cut after the first bytes", b, err)
	}
	within(t, "the app's request to end", cancelled)
}

func TestByteBudget(t *testing.T) {
	lim := testLimits()
	lim.BytesPerHour = 64 << 10
	f := newFixture(t, lim, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(bytes.Repeat([]byte("z"), 128<<10))
	}))
	f.connect(owner("t1", "u1"))
	r, err := f.request(context.Background(), "GET", "t1", "/", nil)
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, r.Body)
	r.Body.Close()
	if err == nil || n >= 128<<10 {
		t.Fatalf("over the budget: read %d bytes, err %v; want it cut", n, err)
	}
	r, err = f.request(context.Background(), "GET", "t1", "/", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusTooManyRequests || r.Header.Get("Retry-After") == "" {
		t.Fatalf("after the budget: %d Retry-After %q", r.StatusCode, r.Header.Get("Retry-After"))
	}
	if got := f.reg.Connected(); len(got) != 1 || got[0].BytesThisHour < 64<<10 {
		t.Fatalf("admin view: %+v", got)
	}
}

// TestHeaderHygiene: what the app receives and what the visitor gets back
// (DESIGN.md "What the app receives", "What visitors receive").
func TestHeaderHygiene(t *testing.T) {
	seen := make(chan http.Header, 1)
	f := newFixture(t, testLimits(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Clone()
		h.Set("Host", r.Host)
		seen <- h
		hd := w.Header()
		hd.Add("Set-Cookie", "a=1; Domain=usercontent.example; Path=/")
		hd.Add("Set-Cookie", "b=2; domain=.usercontent.example; Secure; SameSite=None")
		hd.Add("Set-Cookie", "__Host-rd_visit=forged; Path=/; Secure")
		hd.Set("Cache-Control", "no-store, max-age=0")
		hd.Set("Surrogate-Control", "max-age=60")
		hd.Set("Strict-Transport-Security", "max-age=1")
		hd.Set("Cross-Origin-Resource-Policy", "cross-origin")
		hd.Set("Permissions-Policy", "camera=(self)")
		hd.Set("Clear-Site-Data", `"cookies", "storage"`)
	}))
	f.connect(owner("t1", "u1"))
	r, err := f.request(context.Background(), "GET", "t1", "/", nil,
		"X-Forwarded-For", "203.0.113.5", "X-Forwarded-Host", "evil.example", "Forwarded", "for=1.2.3.4", "X-Real-IP", "1.2.3.4",
		"True-Client-IP", "1.2.3.4", "CF-Connecting-IP", "1.2.3.4", "CF-Ray", "x", "CF-IPCountry", "NZ", "CDN-Loop", "cloudflare",
		"Cookie", "__Host-rd_visit=secret; app=1;  other=2", "Authorization", "Bearer app-token", "Connection", "X-Hop", "X-Hop", "1",
		"Proxy-Authorization", "x", "Keep-Alive", "timeout=5",
		"X-Forwarded-Port", "1234", "X-Forwarded-Prefix", "/evil", "X-Forwarded-Server", "evil", "X-Forwarded-Ssl", "on",
		"X_Forwarded_For", "6.6.6.6", "X_Real_IP", "6.6.6.6", "Client-Ip", "6.6.6.6", "X-Client-Ip", "6.6.6.6")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	h := within(t, "the app's request", seen)
	want := map[string]string{
		"X-Forwarded-For": "192.0.2.1", "X-Forwarded-Proto": "https", "Forwarded": "", "X-Real-Ip": "", "True-Client-Ip": "",
		"Cf-Connecting-Ip": "", "Cf-Ray": "", "Cf-Ipcountry": "", "Cdn-Loop": "", "Cookie": "app=1; other=2",
		"Authorization": "Bearer app-token", "X-Hop": "", "Proxy-Authorization": "", "Keep-Alive": "",
		"Host":             strings.TrimPrefix(f.app.URL, "http://"),
		"X-Forwarded-Port": "", "X-Forwarded-Prefix": "", "X-Forwarded-Server": "", "X-Forwarded-Ssl": "",
		"X_forwarded_for": "", "X_real_ip": "", "Client-Ip": "", "X-Client-Ip": "",
	}
	for k, v := range want {
		if got := strings.Join(h.Values(k), ","); got != v {
			t.Errorf("app got %s %q, want %q", k, got, v)
		}
	}
	if got := h.Get("X-Forwarded-Host"); got != strings.TrimPrefix(f.front.URL, "http://") {
		t.Errorf("X-Forwarded-Host %q: want the tunnel's host, never the visitor's", got)
	}
	if got := r.Header.Values("Set-Cookie"); strings.Join(got, "|") != "a=1; Path=/|b=2; Secure; SameSite=None" {
		t.Errorf("Set-Cookie %q", got)
	}
	for k, v := range map[string]string{"Cache-Control": "private, no-store", "Surrogate-Control": "", "Strict-Transport-Security": "",
		"Cross-Origin-Resource-Policy": "cross-origin", "Permissions-Policy": "camera=(self)", "Clear-Site-Data": ""} {
		if got := strings.Join(r.Header.Values(k), ","); got != v {
			t.Errorf("visitor got %s %q, want %q", k, got, v)
		}
	}
}

func TestHostOnlyCookie(t *testing.T) {
	reserved := func(n string) bool { return n == "__Host-rd_visit" }
	for in, want := range map[string]string{
		"a=1":                                 "a=1",
		"a=1; Domain=x.example":               "a=1",
		"a=1;DOMAIN=x.example;Path=/":         "a=1;Path=/",
		"a=b=c; Path=/; Domain = x; HttpOnly": "a=b=c; Path=/; HttpOnly",
		" __Host-rd_visit =v":                 "",
	} {
		got, ok := hostOnlyCookie(in, reserved)
		if (want == "") == ok || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, ok, want)
		}
	}
}
