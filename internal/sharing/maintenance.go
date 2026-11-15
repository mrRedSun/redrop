package sharing

import (
	"context"
	"os"
	"time"
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
	if ids, err := s.db.ExpiredUploads(ctx); err == nil && len(ids) > 0 {
		// A crashed or failed rollback may have left materialized blobs. Reclaim
		// them before releasing the expired uploads' byte reservations.
		refs, err := s.db.ReferencedBlobs(ctx)
		if err != nil {
			s.log.Error("expired upload references", "error", err)
			return
		}
		if _, _, err := s.blobs.GC(refs, 0); err != nil {
			s.log.Error("expired upload blobs", "error", err)
			return
		}
		for _, id := range ids {
			s.discardUpload(ctx, id)
		}
	}
	s.db.Cleanup(ctx, viewRetention)
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
