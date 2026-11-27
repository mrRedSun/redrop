package main

import (
	"context"
	"sync"

	"github.com/mrRedSun/redrop/pkg/client"
)

// keepLive is client.KeepLive; tests replace it.
var keepLive = client.KeepLive

// liveSession runs keepLive for one watch (DESIGN.md "Live reload"): it
// starts once the watcher is ready, and stop ends it and waits for the
// lease to be released, which is bounded by the client.
type liveSession struct {
	once sync.Once
	stop context.CancelFunc
	done chan struct{}
}

func (l *liveSession) start(ctx context.Context, h client.LiveHolder, id string, onNotice func(client.Notice)) {
	l.once.Do(func() {
		ctx, l.stop = context.WithCancel(ctx)
		l.done = make(chan struct{})
		go func() {
			defer close(l.done)
			keepLive(ctx, h, id, client.LiveOptions{OnNotice: onNotice})
		}()
	})
}

// end stops the session, if it started, and waits for its release.
func (l *liveSession) end() {
	if l.stop == nil {
		return
	}
	l.stop()
	<-l.done
}
