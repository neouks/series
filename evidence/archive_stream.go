package evidence

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/neouks/series/db"
)

// The producer yields one metadata row at a time. Bodies remain in the existing
// content-addressed evidence store; this interface never buffers their contents.
type SnapshotIterator func(func(db.TrafficEvidenceSnapshot) error) error

func snapshotList(values []db.TrafficEvidenceSnapshot) SnapshotIterator {
	return func(visit func(db.TrafficEvidenceSnapshot) error) error {
		for _, v := range values {
			if err := visit(v); err != nil {
				return err
			}
		}
		return nil
	}
}

func (s *Store) CopySnapshotIterator(ctx context.Context, each SnapshotIterator, dest string) error {
	if each == nil {
		return errors.New("nil evidence snapshot iterator")
	}
	return s.DB.WithEvidenceTx(ctx, func(*sql.Tx) error { return s.copySnapshotIterator(ctx, each, dest) })
}

func (s *Store) copySnapshotIterator(ctx context.Context, each SnapshotIterator, dest string) error {
	return each(func(v db.TrafficEvidenceSnapshot) error {
		for _, side := range []string{"request", "response"} {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := func() error {
				hash, length := v.ReqHash, v.ReqLen
				if side == "response" {
					hash, length = v.RespHash, v.RespLen
				}
				path, err := hashPath(s.Dir, hash)
				if err != nil {
					return err
				}
				f, err := os.Open(path)
				if err != nil {
					return err
				}
				defer f.Close()
				target, err := hashPath(dest, hash)
				if err != nil {
					return err
				}
				// Validate while copying once into the private archive destination;
				// do not read/hash the entire source first and then read it again.
				return copyArchiveEvidenceBody(ctx, target, f, hash, length)
			}(); err != nil {
				return err
			}
		}
		return ctx.Err()
	})
}

func copyArchiveEvidenceBody(ctx context.Context, target string, reader io.Reader, hash string, length int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if os.IsExist(err) {
		return verifyFileContext(ctx, target, hash, length)
	}
	if err != nil {
		return err
	}
	owned, statErr := out.Stat()
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(out, h), contextEvidenceReader{ctx: ctx, reader: reader})
	if copyErr == nil && (n != length || hex.EncodeToString(h.Sum(nil)) != hash) {
		copyErr = errors.New("archive evidence body changed during copy")
	}
	syncErr, closeErr := out.Sync(), out.Close()
	runErr := errors.Join(statErr, copyErr, syncErr, closeErr)
	if runErr != nil && owned != nil {
		if current, e := os.Lstat(target); e == nil && os.SameFile(owned, current) {
			runErr = errors.Join(runErr, os.Remove(target))
		}
	}
	return runErr
}

// Keep the existing evidence advisory lock over installation and metadata commit.
// Installed content-addressed bodies retain the existing GC grace behavior on
// metadata failure; never remove a body which another finding may already use.
func (s *Store) WithInstalledSnapshotIterator(ctx context.Context, each SnapshotIterator, source string, restore func() error) error {
	if each == nil || restore == nil {
		return errors.New("nil evidence restore callback")
	}
	return s.DB.WithEvidenceTx(ctx, func(*sql.Tx) error {
		if err := each(func(v db.TrafficEvidenceSnapshot) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if v.ID != db.TrafficSnapshotID(v) {
				return errors.New("归档证据快照元数据哈希不匹配")
			}
			for _, body := range []struct {
				hash   string
				length int64
			}{{v.ReqHash, v.ReqLen}, {v.RespHash, v.RespLen}} {
				if err := func() error {
					path, err := hashPath(source, body.hash)
					if err != nil {
						return err
					}
					file, err := os.Open(path)
					if err != nil {
						return err
					}
					defer file.Close()
					_, err = s.writeBodyContext(ctx, file, body.length, body.hash)
					return err
				}(); err != nil {
					return err
				}
			}
			return ctx.Err()
		}); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return restore()
	})
}

type contextEvidenceReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextEvidenceReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
