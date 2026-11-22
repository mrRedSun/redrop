package sharing

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/internal/store"
)

const uploadTTL = time.Hour // renewed after each successful nonempty chunk

func (s *Service) stagedPath(id string) string { return filepath.Join(s.uploadDir, id) }

// maxUploadBytes allows archive overhead on top of the content limit.
func (s *Service) maxUploadBytes() int64 {
	return s.cfg.MaxVersionBytes + s.cfg.MaxVersionBytes/10 + 10<<20
}

// UploadTarget is what an upload is for, declared when it starts.
type UploadTarget struct {
	// ArtifactID is the artifact the upload is a new version of; "" for a
	// new artifact. Undeclared uploads can't become versions of end-to-end
	// encrypted artifacts.
	ArtifactID string
	// E2E: the content is end-to-end encrypted.
	E2E bool
}

// CreateUpload reserves a staged upload of the declared size. An upload
// declared for an end-to-end encrypted artifact must be declared encrypted,
// and the reverse, so plaintext meant for such an artifact is refused
// before any byte of it arrives (DESIGN.md "End-to-end encrypted links").
func (s *Service) CreateUpload(ctx context.Context, actor Actor, size int64, target UploadTarget) (*Upload, error) {
	if size < 0 {
		return nil, apperr.Invalid("size must be >= 0")
	}
	if target.ArtifactID != "" {
		a, err := s.owned(ctx, actor, target.ArtifactID)
		if err != nil {
			return nil, err
		}
		if err := checkE2ETarget(a, target.E2E); err != nil {
			return nil, err
		}
	}
	if size > s.maxUploadBytes() {
		return nil, apperr.TooLarge("upload exceeds the %s per-version limit", humanBytes(s.cfg.MaxVersionBytes))
	}
	maxOpen := s.cfg.MaxOpenUploads
	if maxOpen == 0 {
		maxOpen = 8
	}
	u, err := s.db.CreateUpload(ctx, actor.UserID, size, store.UploadTarget{ArtifactID: target.ArtifactID, E2E: target.E2E}, uploadTTL, s.cfg.MaxTotalBytes, maxOpen)
	if err != nil {
		return nil, mapStore(err)
	}
	f, err := os.OpenFile(s.stagedPath(u.ID), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		s.discardUpload(context.WithoutCancel(ctx), u.ID)
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

// claimUpload prevents expiry from unlinking a file while a slow chunk still
// holds it open. Such bytes remain reserved until that request ends. It also
// prevents simultaneous chunks or publication of a changing staged file.
func (s *Service) claimUpload(id string) bool {
	s.uploadMu.Lock()
	defer s.uploadMu.Unlock()
	if s.activeUploads[id] {
		return false
	}
	if s.activeUploads == nil {
		s.activeUploads = make(map[string]bool)
	}
	s.activeUploads[id] = true
	return true
}
func (s *Service) releaseUpload(id string) {
	s.uploadMu.Lock()
	delete(s.activeUploads, id)
	s.uploadMu.Unlock()
}

// claimPublisher gives each account at most one publication running or
// waiting for the service-wide slot, so the wait of every other account is
// bounded by one attempt per active account.
func (s *Service) claimPublisher(userID string) bool {
	s.uploadMu.Lock()
	defer s.uploadMu.Unlock()
	if s.activePublishers[userID] {
		return false
	}
	if s.activePublishers == nil {
		s.activePublishers = make(map[string]bool)
	}
	s.activePublishers[userID] = true
	return true
}
func (s *Service) releasePublisher(userID string) {
	s.uploadMu.Lock()
	delete(s.activePublishers, userID)
	s.uploadMu.Unlock()
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
	if !s.claimUpload(id) {
		return nil, apperr.Conflict("upload is already in use; retry from its reported offset")
	}
	defer s.releaseUpload(id)
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
	n, err := io.Copy(f, io.LimitReader(body, limit))
	if err != nil {
		return nil, apperr.Invalid("chunk transfer interrupted; resume from the reported offset")
	}
	var extra [1]byte
	if more, readErr := body.Read(extra[:]); more > 0 {
		return nil, apperr.TooLarge("chunk too large: at most %d bytes at this offset", limit)
	} else if readErr != nil && readErr != io.EOF {
		return nil, apperr.Invalid("chunk transfer interrupted; resume from the reported offset")
	}
	if err := f.Sync(); err != nil {
		return nil, apperr.Internal(err)
	}
	u.Received = offset + n
	if err := s.db.SetUploadReceived(ctx, u.ID, u.Received, uploadTTL); err != nil {
		return nil, apperr.Internal(err)
	}
	return s.GetUpload(ctx, actor, id)
}

// discardUpload removes a consumed or abandoned upload.
func (s *Service) discardUpload(ctx context.Context, id string) {
	if err := os.Remove(s.stagedPath(id)); err != nil && !os.IsNotExist(err) {
		s.log.Error("remove upload", "id", id, "error", err)
		return // retain the reservation until bytes are reclaimed
	}
	if err := s.db.DeleteUpload(ctx, id); err != nil {
		s.log.Error("release upload reservation", "id", id, "error", err)
	}
}
