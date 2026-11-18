package tunnel

import (
	"context"
	"io"
	"net/http"
	"runtime"
	"sync"
	"testing"
)

// The production container has 768 MB of memory. Bodies stream through the
// tunnel in pieces: 300 MB each way, over a real WebSocket, while the heap
// stays far below the body's size.
const (
	streamedBytes = 300 << 20
	heapBound     = 64 << 20 // in use at any sample, every party included
	sampleEvery   = 8 << 20
)

// heapPeak samples the heap in use (server, CLI, app and visitor share this
// process) and keeps the highest value.
type heapPeak struct {
	mu   sync.Mutex
	peak uint64
}

func (h *heapPeak) sample() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	h.mu.Lock()
	h.peak = max(h.peak, m.HeapInuse)
	h.mu.Unlock()
}

// pattern is a reader of n bytes that allocates nothing per read.
type pattern struct{ left int64 }

var block = func() []byte {
	b := make([]byte, 32<<10)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}()

func (p *pattern) Read(b []byte) (int, error) {
	if p.left == 0 {
		return 0, io.EOF
	}
	n := copy(b, block[:min(int64(len(block)), p.left)])
	p.left -= int64(n)
	return n, nil
}

// sampling counts bytes written and samples the heap as they pass.
type sampling struct {
	n, next int64
	h       *heapPeak
}

func (s *sampling) Write(b []byte) (int, error) {
	s.n += int64(len(b))
	if s.n >= s.next {
		s.h.sample()
		s.next += sampleEvery
	}
	return len(b), nil
}

func TestStreamsThreeHundredMegabytesEachWay(t *testing.T) {
	if testing.Short() {
		t.Skip("streams 600 MB")
	}
	lim := testLimits()
	lim.MaxRequestBytes, lim.MaxResponseBytes, lim.BytesPerHour = 1<<30, 1<<30, 2<<30
	peak := &heapPeak{}
	uploaded := make(chan int64, 1)
	f := newFixture(t, lim, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/octet-stream")
			io.Copy(w, &pattern{left: streamedBytes})
		case http.MethodPost:
			s := &sampling{h: peak}
			io.Copy(s, r.Body)
			uploaded <- s.n
		}
	}))
	f.connectWS(owner("t1", "u1"))
	runtime.GC()
	peak.sample()

	r, err := f.request(context.Background(), "GET", "t1", "/download", nil)
	if err != nil {
		t.Fatal(err)
	}
	s := &sampling{h: peak}
	_, err = io.Copy(s, r.Body)
	r.Body.Close()
	if err != nil || s.n != streamedBytes {
		t.Fatalf("download: %d bytes, %v", s.n, err)
	}

	r, err = f.request(context.Background(), "POST", "t1", "/upload", &pattern{left: streamedBytes})
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if n := within(t, "the upload", uploaded); r.StatusCode != 200 || n != streamedBytes {
		t.Fatalf("upload: status %d, the app read %d bytes", r.StatusCode, n)
	}
	if peak.peak > heapBound {
		t.Fatalf("heap in use peaked at %d MiB while streaming %d MiB; bodies are being buffered", peak.peak>>20, streamedBytes>>20)
	}
	t.Logf("heap in use peaked at %d MiB", peak.peak>>20)
}
