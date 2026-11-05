package server

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"

	pgdb "github.com/neouks/series/db"
	"github.com/neouks/series/evidence"
)

func snapshotTaskArchiveStreams(ctx context.Context, pg *pgdb.DB, taskID int64, payload string) (snapshot *pgdb.TaskArchiveSnapshot, runErr error) {
	files := map[string]*os.File{}
	paths := []string{}
	defer func() {
		if runErr == nil {
			for _, file := range files {
				runErr = errors.Join(runErr, file.Sync())
			}
		}
		for _, file := range files {
			runErr = errors.Join(runErr, file.Close())
		}
		if runErr != nil {
			snapshot = nil
			for _, path := range paths {
				runErr = errors.Join(runErr, os.Remove(path))
			}
		}
	}()
	streams := map[string]io.Writer{}
	names := []string{}
	pathsByName := pgdb.TaskArchiveStreamFiles()
	for name := range pathsByName {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path := filepath.Join(payload, filepath.FromSlash(pathsByName[name]))
		if err := os.MkdirAll(filepath.Dir(path), archiveDirMode); err != nil {
			return nil, err
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, archiveFileMode)
		if err != nil {
			return nil, err
		}
		files[name] = file
		paths = append(paths, path)
		streams[name] = file
	}
	return pg.SnapshotTaskArchiveWithStreams(ctx, taskID, streams)
}

func taskArchiveEvidenceIterator(ctx context.Context, snapshot *pgdb.TaskArchiveSnapshot, root string) evidence.SnapshotIterator {
	return func(visit func(pgdb.TrafficEvidenceSnapshot) error) (runErr error) {
		if err := pgdb.ValidateTaskArchiveStreams(snapshot); err != nil {
			return err
		}
		var reader io.Reader
		if relative := snapshot.StreamedTables["traffic_evidence_snapshots"]; relative != "" {
			file, err := openTaskArchiveStreamFile(root, relative)
			if err != nil {
				return err
			}
			defer func() { runErr = errors.Join(runErr, file.Close()) }()
			reader = file
		}
		return pgdb.ForEachArchiveEvidenceSnapshot(ctx, snapshot, reader, visit)
	}
}

func openTaskArchiveStreamFile(root, relative string) (*os.File, error) {
	path := filepath.Join(root, filepath.FromSlash(relative))
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, errors.New("archive stream is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) {
		file.Close()
		return nil, errors.Join(err, errors.New("archive stream file changed while opening"))
	}
	return file, nil
}

type archiveStreamReaders struct {
	files   map[string]*os.File
	readers map[string]io.Reader
}

func (r *archiveStreamReaders) Close() error {
	var err error
	for name, file := range r.files {
		err = errors.Join(err, file.Close())
		delete(r.files, name)
	}
	return err
}

func openTaskArchiveStreams(snapshot *pgdb.TaskArchiveSnapshot, root string) (_ *archiveStreamReaders, runErr error) {
	if err := pgdb.ValidateTaskArchiveStreams(snapshot); err != nil {
		return nil, err
	}
	result := &archiveStreamReaders{files: map[string]*os.File{}, readers: map[string]io.Reader{}}
	defer func() {
		if runErr != nil {
			runErr = errors.Join(runErr, result.Close())
		}
	}()
	for name, relative := range snapshot.StreamedTables {
		file, err := openTaskArchiveStreamFile(root, relative)
		if err != nil {
			return nil, err
		}
		result.files[name] = file
		result.readers[name] = file
	}
	return result, nil
}
