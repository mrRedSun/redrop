package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrRedSun/redrop/pkg/api"
)

func TestPublishRetriesWhileAccountIsBusy(t *testing.T) {
	defer func(d, m, w time.Duration) { publishBusyDelay, publishBusyMax, publishBusyWait = d, m, w }(publishBusyDelay, publishBusyMax, publishBusyWait)
	publishBusyDelay, publishBusyMax, publishBusyWait = time.Millisecond, 2*time.Millisecond, time.Minute
	for _, tc := range []struct {
		name     string
		title    string
		busy     int32
		wantCode int
		wantHits int32
	}{
		{"busy then published", api.CodePublicationBusy, 3, 0, 4},
		{"other conflict is not retried", "Conflict", 3, http.StatusConflict, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if hits.Add(1) <= tc.busy {
					w.WriteHeader(http.StatusConflict)
					json.NewEncoder(w).Encode(map[string]any{"status": 409, "title": tc.title, "detail": "busy"})
					return
				}
				w.WriteHeader(http.StatusCreated)
				json.NewEncoder(w).Encode(api.Artifact{ID: "a1", Version: 1})
			}))
			defer server.Close()
			c := New(server.URL, "rdp_test")
			_, err := c.CreateVersion(context.Background(), "a1", &api.PublishRequest{UploadID: "u"})
			if StatusOf(err) != tc.wantCode || hits.Load() != tc.wantHits {
				t.Fatalf("err=%v hits=%d, want status %d after %d requests", err, hits.Load(), tc.wantCode, tc.wantHits)
			}
		})
	}
}

func TestPublishBusyWaitIsBounded(t *testing.T) {
	defer func(d, m, w time.Duration) { publishBusyDelay, publishBusyMax, publishBusyWait = d, m, w }(publishBusyDelay, publishBusyMax, publishBusyWait)
	publishBusyDelay, publishBusyMax, publishBusyWait = time.Millisecond, time.Millisecond, 20*time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{"status": 409, "title": api.CodePublicationBusy, "detail": "busy"})
	}))
	defer server.Close()
	_, err := New(server.URL, "rdp_test").CreateArtifact(context.Background(), &api.CreateArtifactRequest{})
	if StatusOf(err) != http.StatusConflict {
		t.Fatalf("err=%v, want the busy conflict after the wait", err)
	}
}
