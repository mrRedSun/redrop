package server

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	viewRetention = 180 * 24 * time.Hour
	dbBackupsKept = 7
)

// janitor purges expired content, stale uploads, unreferenced blobs, and
// keeps daily database snapshots.
func (s *Server) janitor(ctx context.Context) {
	minute := time.NewTicker(time.Minute)
	defer minute.Stop()
	lastGC, lastBackup := time.Time{}, time.Time{}
	for {
		s.sweep(ctx)
		if time.Since(lastGC) > time.Hour {
			s.gc(ctx)
			lastGC = time.Now()
		}
		if time.Since(lastBackup) > 24*time.Hour {
			s.backupDB(ctx)
			lastBackup = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-minute.C:
		}
	}
}

func (s *Server) sweep(ctx context.Context) {
	if n, err := s.db.PurgeArtifacts(ctx, s.cfg.ExpiredRetention, s.cfg.ViewWindowLimited); err != nil {
		s.log.Error("purge artifacts", "error", err)
	} else if n > 0 {
		s.log.Info("purged expired or exhausted artifacts", "count", n)
	}
	ids, err := s.db.ExpiredUploads(ctx)
	if err == nil {
		for _, id := range ids {
			os.Remove(s.stagedPath(id))
			s.db.DeleteUpload(ctx, id)
		}
	}
	s.db.Cleanup(ctx, viewRetention)
}

func (s *Server) gc(ctx context.Context) {
	// Staged files without an upload record (e.g. after a crash).
	if entries, err := os.ReadDir(s.uploadDir); err == nil {
		for _, e := range entries {
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > time.Hour && !s.db.UploadExists(ctx, e.Name()) {
				os.Remove(filepath.Join(s.uploadDir, e.Name()))
			}
		}
	}
	s.blobs.CleanTmp(6 * time.Hour)
	refs, err := s.db.ReferencedBlobs(ctx)
	if err != nil {
		s.log.Error("blob gc", "error", err)
		return
	}
	if n, freed, err := s.blobs.GC(refs, time.Hour); err != nil {
		s.log.Error("blob gc", "error", err)
	} else if n > 0 {
		s.log.Info("removed unreferenced blobs", "count", n, "bytes", freed)
	}
}

func (s *Server) backupDB(ctx context.Context) {
	dir := filepath.Join(s.cfg.DataDir, "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.log.Error("db backup", "error", err)
		return
	}
	name := filepath.Join(dir, "redrop-"+time.Now().UTC().Format("20060102")+".db")
	if _, err := os.Stat(name); err == nil {
		return
	}
	if err := s.db.Backup(ctx, name); err != nil {
		s.log.Error("db backup", "error", err)
		return
	}
	os.Chmod(name, 0o600)
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "redrop-") && strings.HasSuffix(e.Name(), ".db") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for len(names) > dbBackupsKept {
		os.Remove(filepath.Join(dir, names[0]))
		names = names[1:]
	}
}
