package sharing

import (
	"context"
	"io"
	"time"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/pkg/api"
)

// A publication owns the new blobs and unpacked-byte reservation until commit.
// Each account has at most one publication running or waiting. The service
// admits one materialization at a time, including rollback and GC, so a
// failed attempt cannot delete bytes another publisher is about to reference.
type publication struct {
	s         *Service
	ctx       context.Context
	actor     Actor
	uploadID  string
	admitted  bool // holds the service-wide slot
	created   map[string]struct{}
	committed bool
}

// beginPublication claims the account's publication turn and the upload. It
// does not wait: a second publication by the same account is refused at once,
// so one account cannot queue many attempts ahead of other users.
func (s *Service) beginPublication(ctx context.Context, actor Actor, uploadID string) (*publication, error) {
	if err := ctx.Err(); err != nil {
		return nil, apperr.Internal(err)
	}
	if !s.claimPublisher(actor.UserID) {
		return nil, apperr.Conflict("another publication by this account is still in progress; retry when it finishes").WithCode(api.CodePublicationBusy)
	}
	if !s.claimUpload(uploadID) {
		s.releasePublisher(actor.UserID)
		return nil, apperr.Conflict("upload is already in use; retry from its reported offset")
	}
	p := &publication{s: s, ctx: ctx, actor: actor, uploadID: uploadID, created: make(map[string]struct{})}
	up, err := s.db.UploadByID(ctx, actor.UserID, uploadID)
	if err != nil {
		p.release()
		return nil, mapStore(err)
	}
	if up.Materialized != 0 {
		p.release()
		return nil, apperr.Conflict("upload cleanup is pending; wait for expiry before starting another upload")
	}
	return p, nil
}

// admit waits for the service-wide slot. Waiting requests do no
// decompression and can leave through their context.
func (p *publication) admit() error {
	select {
	case p.s.publication <- struct{}{}:
		p.admitted = true
		return nil
	case <-p.ctx.Done():
		return apperr.Internal(p.ctx.Err())
	}
}

func (p *publication) put(r io.Reader, size int64) (string, int64, error) {
	if size < 0 || size > p.s.cfg.MaxVersionBytes {
		return "", 0, apperr.TooLarge("content exceeds the per-version limit")
	}
	if err := p.s.db.ReserveMaterialization(p.ctx, p.actor.UserID, p.uploadID, size, p.s.cfg.MaxTotalBytes); err != nil {
		return "", 0, mapStore(err)
	}
	return p.s.blobs.PutTracked(contextReader{ctx: p.ctx, r: r}, size, p.created)
}

// finish ends the attempt with its result. A committed upload is consumed. A
// failed attempt removes its new unreferenced blobs and releases its unpacked
// reservation. After a deterministic client error (see releasesUpload), and
// only if that cleanup succeeded, the upload is deleted too, which releases
// its bytes and session slot: the CLI never resumes a rejected upload, and a
// replay would fail the same way. Cancellation, conflicts and internal errors
// keep the upload until its idle expiry.
func (p *publication) finish(err error) {
	defer p.release()
	// Cleanup must finish even when the publishing request was cancelled.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(p.ctx), 30*time.Second)
	defer cancel()
	if p.committed {
		p.s.discardUpload(ctx, p.uploadID)
		return
	}
	if !p.rollback(ctx) {
		return
	}
	if p.ctx.Err() == nil && releasesUpload(err) {
		p.s.discardUpload(ctx, p.uploadID)
	}
}

// rollback removes the blobs this attempt installed that no file references,
// then resets the unpacked reservation. It reports whether both succeeded.
func (p *publication) rollback(ctx context.Context) bool {
	if !p.admitted {
		return true // nothing was reserved or written
	}
	if len(p.created) > 0 {
		hashes := make([]string, 0, len(p.created))
		for sum := range p.created {
			hashes = append(hashes, sum)
		}
		refs, err := p.s.db.ReferencedBlobsAmong(ctx, hashes)
		if err != nil {
			p.s.log.Error("publication rollback references", "error", err)
			return false
		}
		for _, sum := range hashes {
			if _, ok := refs[sum]; ok {
				continue
			}
			if err := p.s.blobs.Remove(sum); err != nil {
				p.s.log.Error("publication rollback blob", "error", err)
				return false
			}
		}
	}
	if err := p.s.db.ResetMaterialization(ctx, p.actor.UserID, p.uploadID); err != nil {
		p.s.log.Error("publication rollback reservation", "error", err)
		return false
	}
	return true
}

func (p *publication) release() {
	if p.admitted {
		<-p.s.publication
		p.admitted = false
	}
	p.s.releaseUpload(p.uploadID)
	p.s.releasePublisher(p.actor.UserID)
}

// releasesUpload reports whether a failed publication's upload should be
// deleted: the request itself was rejected (invalid content, archive or
// settings, a limit or quota, an unknown artifact), so retrying the same
// upload cannot succeed. Conflicts (an incomplete or busy upload) and
// internal errors keep it.
func releasesUpload(err error) bool {
	if err == nil {
		return false
	}
	switch e := apperr.As(err); e.Kind {
	case apperr.KindInvalid, apperr.KindTooLarge, apperr.KindNotFound, apperr.KindForbidden, apperr.KindGone:
		return true
	case apperr.KindConflict:
		// End-to-end content encrypted for a version number another version
		// took can never be published; the client encrypts again.
		return e.Code == codeVersionConflict
	}
	return false
}
