// Package janitor runs periodic maintenance: purging expired content,
// garbage-collecting blobs, and keeping daily database snapshots.
package janitor

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mrRedSun/redrop/internal/sharing"
	"github.com/mrRedSun/redrop/internal/store"
)

const (
	sweepEvery    = time.Minute
	gcEvery       = time.Hour
	backupEvery   = 24 * time.Hour
	backupsKept   = 7
	backupPrefix  = "redrop-"
	backupSuffix  = ".db"
	backupDirName = "backups"
)

type Janitor struct {
	sharing *sharing.Service
	db      *store.Store
	dataDir string
	log     *slog.Logger
}

func New(sh *sharing.Service, db *store.Store, dataDir string, log *slog.Logger) *Janitor {
	return &Janitor{sharing: sh, db: db, dataDir: dataDir, log: log}
}

// Run blocks until ctx is cancelled.
func (j *Janitor) Run(ctx context.Context) {
	tick := time.NewTicker(sweepEvery)
	defer tick.Stop()
	var lastGC, lastBackup time.Time
	for {
		j.sharing.Sweep(ctx)
		if time.Since(lastGC) >= gcEvery {
			j.sharing.CollectGarbage(ctx)
			lastGC = time.Now()
		}
		if time.Since(lastBackup) >= backupEvery {
			if err := j.Backup(ctx); err != nil {
				j.log.Error("database snapshot failed", "error", err)
			}
			lastBackup = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Backup writes today's consistent snapshot and keeps the newest seven.
func (j *Janitor) Backup(ctx context.Context) error {
	dir := filepath.Join(j.dataDir, backupDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	name := filepath.Join(dir, backupPrefix+time.Now().UTC().Format("20060102")+backupSuffix)
	if _, err := os.Stat(name); err == nil {
		return nil
	}
	if err := j.db.Backup(ctx, name); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var snapshots []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), backupPrefix) && strings.HasSuffix(e.Name(), backupSuffix) {
			snapshots = append(snapshots, e.Name())
		}
	}
	sort.Strings(snapshots)
	for len(snapshots) > backupsKept {
		os.Remove(filepath.Join(dir, snapshots[0]))
		snapshots = snapshots[1:]
	}
	return nil
}
