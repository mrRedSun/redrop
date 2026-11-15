package sharing

import (
	"context"
	"os"
	"time"

	"github.com/mrRedSun/redrop/internal/store"
)

const (
	viewRetention   = 180 * 24 * time.Hour
	blobGraceAge    = time.Hour     // protects blobs written by in-flight publishes
	tmpRetention    = 6 * time.Hour // abandoned temp files
	orphanUploadAge = time.Hour
)

// Sweep purges expired or used-up artifacts, expired uploads, and old logs.
func (s *Service) Sweep(ctx context.Context) {
	select {
	case s.publication <- struct{}{}:
		defer func() { <-s.publication }()
	case <-ctx.Done():
		return
	}
	if n, err := s.db.PurgeArtifacts(ctx, s.cfg.ExpiredRetention, s.cfg.ViewWindowLimited); err != nil {
		s.log.Error("purge artifacts", "error", err)
	} else if n > 0 {
		s.log.Info("purged expired or used-up artifacts", "count", n)
	}
	if expired, err := s.db.ExpiredUploads(ctx); err != nil {
		s.log.Error("expired uploads", "error", err)
	} else if len(expired) > 0 {
		s.releaseExpiredUploads(ctx, expired)
	}
	s.db.Cleanup(ctx, viewRetention)
}

// releaseExpiredUploads removes expired uploads and their reservations. Only
// an upload with unpacked bytes (a crashed or failed rollback) may have left
// blobs behind; those are reclaimed by a full GC first, and kept while it
// fails. Uploads without unpacked bytes never wait for that GC.
func (s *Service) releaseExpiredUploads(ctx context.Context, expired []store.ExpiredUpload) {
	needGC := false
	for _, u := range expired {
		needGC = needGC || u.Materialized != 0
	}
	reclaimed := false // a full GC succeeded in this sweep
	if needGC {
		refs, err := s.db.ReferencedBlobs(ctx)
		if err == nil {
			_, _, err = s.blobs.GC(refs, 0)
		}
		if err != nil {
			s.log.Error("expired upload blobs", "error", err)
		}
		reclaimed = err == nil
	}
	for _, u := range expired {
		if !s.claimUpload(u.ID) {
			continue // a chunk or publication is using it
		}
		if still, materialized, err := s.db.UploadExpired(ctx, u.ID); err == nil && still && (materialized == 0 || reclaimed) {
			s.discardUpload(ctx, u.ID)
		}
		s.releaseUpload(u.ID)
	}
}

// CollectGarbage removes orphaned staging files and unreferenced blobs.
func (s *Service) CollectGarbage(ctx context.Context) {
	select {
	case s.publication <- struct{}{}:
		defer func() { <-s.publication }()
	case <-ctx.Done():
		return
	}
	if entries, err := os.ReadDir(s.uploadDir); err == nil {
		for _, e := range entries {
			info, err := e.Info()
			if err == nil && time.Since(info.ModTime()) > orphanUploadAge && !s.db.UploadExists(ctx, e.Name()) {
				os.Remove(s.stagedPath(e.Name()))
			}
		}
	}
	s.blobs.CleanTmp(tmpRetention)
	refs, err := s.db.ReferencedBlobs(ctx)
	if err != nil {
		s.log.Error("blob gc", "error", err)
		return
	}
	if n, freed, err := s.blobs.GC(refs, blobGraceAge); err != nil {
		s.log.Error("blob gc", "error", err)
	} else if n > 0 {
		s.log.Info("removed unreferenced blobs", "count", n, "bytes", freed)
	}
}
