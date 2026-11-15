package sharing

import (
	"context"
	"io"
	"time"

	"github.com/mrRedSun/redrop/internal/apperr"
)

// A publication owns the new blobs and unpacked-byte reservation until commit.
// The service admits one attempt at a time, including rollback and GC, so a
// failed attempt cannot delete bytes another publisher is about to reference.
type publication struct {
	s         *Service
	ctx       context.Context
	actor     Actor
	uploadID  string
	created   map[string]struct{}
	committed bool
}

func (s *Service) beginPublication(ctx context.Context, actor Actor, uploadID string) (*publication, error) {
	if err := ctx.Err(); err != nil {
		return nil, apperr.Internal(err)
	}
	select {
	case s.publication <- struct{}{}:
	case <-ctx.Done():
		return nil, apperr.Internal(ctx.Err())
	}
	if !s.claimUpload(uploadID) {
		<-s.publication
		return nil, apperr.Conflict("upload is already in use; retry from its reported offset")
	}
	up, err := s.db.UploadByID(ctx, actor.UserID, uploadID)
	if err != nil {
		s.releaseUpload(uploadID)
		<-s.publication
		return nil, mapStore(err)
	}
	if up.Materialized != 0 {
		s.releaseUpload(uploadID)
		<-s.publication
		return nil, apperr.Conflict("upload cleanup is pending; wait for expiry before starting another upload")
	}
	return &publication{s: s, ctx: ctx, actor: actor, uploadID: uploadID, created: make(map[string]struct{})}, nil
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

func (p *publication) close() {
	defer func() { p.s.releaseUpload(p.uploadID); <-p.s.publication }()
	// Cleanup must finish even when the publishing request was cancelled.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(p.ctx), 30*time.Second)
	defer cancel()
	if p.committed {
		p.s.discardUpload(ctx, p.uploadID)
		return
	}
	refs, err := p.s.db.ReferencedBlobs(ctx)
	if err != nil {
		p.s.log.Error("publication rollback references", "error", err)
		return
	}
	for sum := range p.created {
		if _, ok := refs[sum]; ok {
			continue
		}
		if err := p.s.blobs.Remove(sum); err != nil {
			p.s.log.Error("publication rollback blob", "error", err)
			return
		}
	}
	if err := p.s.db.ResetMaterialization(ctx, p.actor.UserID, p.uploadID); err != nil {
		p.s.log.Error("publication rollback reservation", "error", err)
	}
}
