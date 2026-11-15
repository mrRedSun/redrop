package sharing

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"strconv"
	"strings"

	"github.com/mrRedSun/redrop/internal/apperr"
)

// contextReader checks cancellation on both compressed input and decompressed
// output, including tar.Reader's otherwise invisible metadata and discard work.
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// archiveWorkReader sees raw tar blocks before archive/tar hides PAX and GNU
// extension headers. It counts every physical header, not just returned files.
// archive/tar remains responsible for checksums, names, formats and semantics.
type archiveWorkReader struct {
	r            io.Reader
	remaining    int64
	maxEntries   int
	entries      int
	header       [512]byte
	headerBytes  int
	bodyLeft     int64 // includes padding to the next physical tar header
	metadata     []byte
	metadataLeft int64
	metadataType byte
	nextName     string // PAX/GNU path may turn a legacy NUL-type header into a directory
}

func (r *archiveWorkReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.remaining == 0 {
		return 0, apperr.TooLarge("bundle exceeds the decompression work byte limit")
	}
	nmax := min(int64(len(p)), r.remaining)
	if r.bodyLeft > 0 {
		nmax = min(nmax, r.bodyLeft)
	} else {
		nmax = min(nmax, int64(len(r.header)-r.headerBytes))
	}
	n, err := r.r.Read(p[:nmax])
	r.remaining -= int64(n)
	if r.bodyLeft > 0 {
		r.bodyLeft -= int64(n)
		if r.metadataLeft > 0 {
			take := min(int64(n), r.metadataLeft)
			r.metadata = append(r.metadata, p[:take]...)
			r.metadataLeft -= take
			if r.metadataLeft == 0 {
				if metaErr := r.finishMetadata(); metaErr != nil {
					return 0, metaErr
				}
			}
		}
		return n, err
	}
	copy(r.header[r.headerBytes:], p[:n])
	r.headerBytes += n
	if r.headerBytes == len(r.header) {
		r.headerBytes = 0
		if r.header != [512]byte{} {
			r.entries++
			if r.entries > r.maxEntries {
				return 0, apperr.TooLarge("bundle has more than %d archive entries, including metadata and directories", r.maxEntries)
			}
			size, ok := tarPhysicalSize(r.header[124:136])
			if !ok {
				return 0, apperr.Invalid("invalid archive entry size")
			}
			kind := r.header[156]
			switch kind {
			case tar.TypeXHeader, tar.TypeXGlobalHeader, tar.TypeGNULongName:
				// Go's tar parser uses the same 1 MiB metadata ceiling. Inspect
				// framing overrides before they can desynchronize this counter.
				if size > 1<<20 {
					return 0, apperr.TooLarge("archive metadata exceeds 1 MiB")
				}
				r.metadata = make([]byte, 0, size)
				r.metadataLeft, r.metadataType = size, kind
			case tar.TypeDir:
				size = 0 // archive/tar ignores directory size fields
			case tar.TypeRegA:
				name := string(bytes.SplitN(r.header[:100], []byte{0}, 2)[0])
				if r.nextName != "" {
					name = r.nextName
				}
				if strings.HasSuffix(name, "/") {
					size = 0
				}
				r.nextName = ""
			case tar.TypeReg:
				r.nextName = ""
			case tar.TypeGNULongLink:
				// The next link is rejected by the content-type validator.
			case tar.TypeGNUSparse:
				return 0, apperr.Invalid("sparse archive files are not supported")
			}
			// Rounding up must not overflow. The total read budget still applies.
			if size > (1<<63-1)-511 {
				return 0, apperr.TooLarge("archive entry exceeds the decompression work byte limit")
			}
			r.bodyLeft = (size + 511) / 512 * 512
		}
	}
	return n, err
}

func (r *archiveWorkReader) finishMetadata() error {
	defer func() { r.metadata = nil }()
	if r.metadataType == tar.TypeGNULongName {
		r.nextName = string(bytes.SplitN(r.metadata, []byte{0}, 2)[0])
		return nil
	}
	data := r.metadata
	var nextName string
	for len(data) > 0 {
		space := bytes.IndexByte(data, ' ')
		if space <= 0 {
			return apperr.Invalid("corrupt archive metadata")
		}
		n, err := strconv.Atoi(string(data[:space]))
		if err != nil || n <= space+1 || n > len(data) || data[n-1] != '\n' {
			return apperr.Invalid("corrupt archive metadata")
		}
		key, value, ok := strings.Cut(string(data[space+1:n-1]), "=")
		if !ok {
			return apperr.Invalid("corrupt archive metadata")
		}
		// PAX size and sparse maps can change physical body framing after a
		// header is read. They are unnecessary for bounded ordinary bundles.
		if key == "size" || strings.HasPrefix(key, "GNU.sparse.") {
			return apperr.Invalid("archive size overrides and sparse files are not supported")
		}
		if key == "path" {
			nextName = value
		}
		data = data[n:]
	}
	if r.metadataType == tar.TypeXHeader && nextName != "" {
		r.nextName = nextName
	} else if r.metadataType == tar.TypeXGlobalHeader {
		r.nextName = ""
	}
	return nil
}

func tarPhysicalSize(field []byte) (int64, bool) {
	if field[0]&0x80 != 0 { // positive GNU base-256, sign bit is the next bit
		if field[0] != 0x80 {
			return 0, false
		}
		for _, b := range field[1 : len(field)-8] {
			if b != 0 {
				return 0, false
			}
		}
		n := binary.BigEndian.Uint64(field[len(field)-8:])
		return int64(n), n <= 1<<63-1
	}
	text := strings.Trim(string(field), " \x00")
	if text == "" {
		return 0, true
	}
	n, err := strconv.ParseInt(text, 8, 64)
	return n, err == nil && n >= 0
}

func (s *Service) archiveEntryLimit() int {
	// The independent physical-entry cap allows directory and PAX headers while
	// preventing empty entries from scaling work with compressed input size.
	return max(128, 4*s.cfg.MaxFiles)
}
func (s *Service) archiveReadLimit() int64 {
	// 4 KiB per physical entry for headers/padding/metadata, plus 1 MiB slack.
	return s.cfg.MaxVersionBytes + int64(s.archiveEntryLimit())*4096 + 1<<20
}
