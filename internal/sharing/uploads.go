package sharing

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/mrRedSun/redrop/internal/apperr"
)

const uploadTTL = 24 * time.Hour

func (s *Service) stagedPath(id string) string { return filepath.Join(s.uploadDir, id) }

// maxUploadBytes allows archive overhead on top of the content limit.
func (s *Service) maxUploadBytes() int64 {
	return s.cfg.MaxVersionBytes + s.cfg.MaxVersionBytes/10 + 10<<20
}

// CreateUpload reserves a staged upload of the declared size.
func (s *Service) CreateUpload(ctx context.Context, actor Actor, size int64) (*Upload, error) {
	if size < 0 {
		return nil, apperr.Invalid("size must be >= 0")
	}
	if size > s.maxUploadBytes() {
		return nil, apperr.TooLarge("upload exceeds the %s per-version limit", humanBytes(s.cfg.MaxVersionBytes))
	}
	used, err := s.db.UsedBytes(ctx, actor.UserID)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	// Early, approximate check; the authoritative check runs at publish time.
	if actor.Quota > 0 && used+size > actor.Quota+s.cfg.MaxVersionBytes/10 {
		return nil, apperr.TooLarge("storage quota exceeded: %s of %s used", humanBytes(used), humanBytes(actor.Quota))
	}
	u, err := s.db.CreateUpload(ctx, actor.UserID, size, uploadTTL)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	f, err := os.OpenFile(s.stagedPath(u.ID), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	f.Close()
	return s.uploadView(u), nil
}

func (s *Service) GetUpload(ctx context.Context, actor Actor, id string) (*Upload, error) {
	u, err := s.db.UploadByID(ctx, actor.UserID, id)
	if err != nil {
		return nil, apperr.NotFound("upload not found or expired")
	}
	return s.uploadView(u), nil
}

// OffsetMismatchError tells clients where to resume. It unwraps to a
// Conflict application error.
type OffsetMismatchError struct{ Received int64 }

func (e *OffsetMismatchError) Error() string {
	return fmt.Sprintf("offset mismatch: server has %d bytes; resume from there", e.Received)
}

func (e *OffsetMismatchError) Unwrap() error { return apperr.Conflict("%s", e.Error()) }

// AppendChunk writes body at offset, which must equal the bytes received so far.
func (s *Service) AppendChunk(ctx context.Context, actor Actor, id string, offset int64, body io.Reader) (*Upload, error) {
	u, err := s.GetUpload(ctx, actor, id)
	if err != nil {
		return nil, err
	}
	if offset != u.Received {
		return nil, &OffsetMismatchError{Received: u.Received}
	}
	f, err := os.OpenFile(s.stagedPath(u.ID), os.O_WRONLY, 0o600)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	defer f.Close()
	// Discard any partial bytes from an interrupted earlier attempt.
	if err := f.Truncate(offset); err != nil {
		return nil, apperr.Internal(err)
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, apperr.Internal(err)
	}
	limit := s.cfg.ChunkSize
	if rem := u.Size - offset; rem < limit {
		limit = rem
	}
	n, err := io.Copy(f, io.LimitReader(body, limit+1))
	if err != nil {
		return nil, apperr.Invalid("chunk transfer interrupted; resume from the reported offset")
	}
	if n > limit {
		return nil, apperr.TooLarge("chunk too large: at most %d bytes at this offset", limit)
	}
	if err := f.Sync(); err != nil {
		return nil, apperr.Internal(err)
	}
	u.Received = offset + n
	if err := s.db.SetUploadReceived(ctx, u.ID, u.Received); err != nil {
		return nil, apperr.Internal(err)
	}
	return u, nil
}

// discardUpload removes a consumed or abandoned upload.
func (s *Service) discardUpload(ctx context.Context, id string) {
	os.Remove(s.stagedPath(id))
	s.db.DeleteUpload(ctx, id)
}
