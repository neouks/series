package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
	"modernc.org/sqlite"
)

const recoveryRecordBytes = 64 * 1024 * 1024 // Same per-record scale as the official reader.

var errRecoveryMetadata = errors.New("invalid recovery metadata")

// Only derived offsets and delivery/skill metadata live here. No message body,
// credential or reconstructed model answer is copied into the SQLite index.
// Original JSONL remains the sole source of conversation text and signatures.
type recoveryHead struct {
	Version        int       `json:"version"`
	Session        string    `json:"session"`
	Identity       string    `json:"identity"`
	HeadBytes      int       `json:"head_bytes"`
	HeadHash       string    `json:"head_hash"`
	Size           int64     `json:"size"`
	LastOffset     int64     `json:"last_offset"`
	LastBytes      int       `json:"last_bytes"`
	LastHash       string    `json:"last_hash"`
	LastUUID       string    `json:"last_uuid"`
	Usage          llm.Usage `json:"usage"`
	Boundary       bool      `json:"boundary"`
	BoundaryOffset int64     `json:"boundary_offset"`
	BoundaryUsage  llm.Usage `json:"boundary_usage"`
}

type recoveryMetrics struct {
	IndexedBytes, RestoredBytes, VerifiedBytes int64
	Messages                                   int
}
type recoveryJournal struct {
	mu             sync.Mutex
	path, id       string
	db             *sql.DB
	lease          *os.File
	head           recoveryHead
	metrics        recoveryMetrics
	closed         bool
	mustHaveSource bool
}

func newRecoveryJournal(path, id string) (*recoveryJournal, error) {
	if id == "" || id == "." || id == ".." || filepath.Base(id) != id || strings.ContainsAny(id, "/\\\x00") {
		return nil, fmt.Errorf("invalid transcript session identity")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	lease, err := lockRecoveryFile(path + ".series-recovery.lock")
	if err != nil {
		return nil, err
	}
	j := &recoveryJournal{path: path, id: id, lease: lease}
	if err = j.openIndex(); err != nil {
		var sqliteErr *sqlite.Error
		if errors.As(err, &sqliteErr) && (sqliteErr.Code()&255 == 11 || sqliteErr.Code()&255 == 26) {
			j.mustHaveSource = true
			// The file is a rebuildable index, never an audit log. Quarantine only
			// this exact regular sidecar; source validation still precedes Prompt.
			index := path + ".series-recovery.sqlite"
			if renameErr := os.Rename(index, index+".corrupt"); renameErr == nil {
				err = j.openIndex()
			}
		}
		if err != nil {
			unlockRecoveryFile(lease)
			return nil, err
		}
	}
	return j, nil
}

func (j *recoveryJournal) openIndex() error {
	path := j.path + ".series-recovery.sqlite"
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("recovery index is not a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	u := url.URL{Scheme: "file", Path: path}
	db, err := sql.Open("sqlite", u.String()+"?_pragma=busy_timeout(1000)")
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err = db.Exec(`PRAGMA journal_mode=DELETE; PRAGMA cache_size=-2048; PRAGMA mmap_size=0;
 CREATE TABLE IF NOT EXISTS head(singleton INTEGER PRIMARY KEY CHECK(singleton=1),value TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS worker_requests(id TEXT PRIMARY KEY,source_offset INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS skills(name TEXT PRIMARY KEY,source_offset INTEGER NOT NULL);
 CREATE INDEX IF NOT EXISTS skills_by_source ON skills(source_offset,name);
 CREATE TABLE IF NOT EXISTS pending_skills(call_id TEXT PRIMARY KEY,name TEXT NOT NULL,source_offset INTEGER NOT NULL);`); err != nil {
		db.Close()
		return err
	}
	j.db = db
	var hasHead bool
	if err = db.QueryRow(`SELECT EXISTS(SELECT 1 FROM head WHERE singleton=1)`).Scan(&hasHead); err != nil {
		db.Close()
		return err
	}
	if !hasHead {
		if info, e := os.Lstat(path + ".corrupt"); e == nil && info.Mode().IsRegular() {
			j.mustHaveSource = true
		}
	}
	return nil
}

func (j *recoveryJournal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	j.closed = true
	err := j.db.Close()
	unlockRecoveryFile(j.lease)
	return err
}

func (j *recoveryJournal) readHead(ctx context.Context) (recoveryHead, error) {
	var raw string
	var h recoveryHead
	err := j.db.QueryRowContext(ctx, `SELECT value FROM head WHERE singleton=1`).Scan(&raw)
	if err == sql.ErrNoRows {
		return recoveryHead{Version: 1, Session: j.id}, nil
	}
	if err != nil {
		return h, err
	}
	if err = json.Unmarshal([]byte(raw), &h); err != nil {
		return h, errRecoveryMetadata
	}
	if h.Version != 1 || h.Session != j.id || h.Size < 0 || h.LastOffset < 0 || h.LastBytes < 0 || h.LastOffset+int64(h.LastBytes) != h.Size || h.BoundaryOffset < 0 || h.BoundaryOffset > h.Size || h.HeadBytes < 0 || h.HeadBytes > 65536 || int64(h.HeadBytes) > h.Size {
		return h, errRecoveryMetadata
	}
	return h, nil
}

func recoveryHashAt(f *os.File, offset int64, size int) (string, error) {
	h := sha256.New()
	if _, err := io.CopyN(h, io.NewSectionReader(f, offset, int64(size)), int64(size)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func openRecoverySource(path string) (*os.File, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("transcript is not a regular file")
	}
	f, err := openGrepRegular(path)
	if err != nil {
		return nil, nil, err
	}
	actual, err := f.Stat()
	if err != nil || !actual.Mode().IsRegular() || !os.SameFile(info, actual) {
		f.Close()
		return nil, nil, fmt.Errorf("transcript changed while opening")
	}
	return f, actual, nil
}

// The index assumes the canonical SDK file is append-only. Inode/handle identity,
// size, a bounded head seal and the last indexed record seal detect replacement,
// truncation and overwritten append anchors. They are not a whole-file tamper
// proof when an external writer deliberately rewrites an uninspected old middle.
func (j *recoveryJournal) syncSource(ctx context.Context) (*os.File, recoveryHead, error) {
	j.metrics.IndexedBytes = 0
	j.metrics.VerifiedBytes = 0
	h, err := j.readHead(ctx)
	forceReset := errors.Is(err, errRecoveryMetadata)
	if err != nil && !forceReset {
		return nil, h, err
	}
	if forceReset {
		h = recoveryHead{Version: 1, Session: j.id}
	}
	f, info, err := openRecoverySource(j.path)
	if errors.Is(err, os.ErrNotExist) && h.Size == 0 && !forceReset && !j.mustHaveSource {
		j.head = h
		return nil, h, nil
	}
	if err != nil {
		return nil, h, err
	}
	fail := func(e error) (*os.File, recoveryHead, error) { f.Close(); return nil, h, e }
	identity, err := recoveryFileIdentity(f, info)
	if err != nil {
		return fail(err)
	}
	reset := forceReset || h.Identity != identity || h.Size > info.Size()
	if !reset && h.Size > 0 {
		j.metrics.VerifiedBytes += int64(h.HeadBytes + h.LastBytes)
		headHash, e := recoveryHashAt(f, 0, h.HeadBytes)
		if e != nil {
			return fail(e)
		}
		lastHash, e := recoveryHashAt(f, h.LastOffset, h.LastBytes)
		if e != nil {
			return fail(e)
		}
		reset = headHash != h.HeadHash || lastHash != h.LastHash
	}
	if reset {
		// Reset is transactional. A malformed replacement cannot leave a fresh
		// empty checkpoint that would allow the model to repeat earlier actions.
		h = recoveryHead{Version: 1, Session: j.id, Identity: identity}
	}
	if h.Size == info.Size() && !reset {
		j.head = h
		return f, h, nil
	}
	reader := bufio.NewReaderSize(io.NewSectionReader(f, h.Size, info.Size()-h.Size), 64*1024)
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return fail(err)
	}
	defer func() {
		if tx != nil {
			tx.Rollback()
		}
	}()
	if reset {
		if _, err = tx.ExecContext(ctx, `DELETE FROM worker_requests; DELETE FROM skills; DELETE FROM pending_skills; DELETE FROM head;`); err != nil {
			return fail(err)
		}
	}
	start := h.Size
	records := 0
	commit := func() error {
		h.HeadBytes = int(min(h.Size, 65536))
		j.metrics.VerifiedBytes += int64(h.HeadBytes)
		var e error
		h.HeadHash, e = recoveryHashAt(f, 0, h.HeadBytes)
		if e != nil {
			return e
		}
		raw, e := json.Marshal(h)
		if e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO head(singleton,value) VALUES(1,?) ON CONFLICT(singleton) DO UPDATE SET value=excluded.value`, string(raw)); e != nil {
			return e
		}
		if e = tx.Commit(); e != nil {
			return e
		}
		tx = nil
		return nil
	}
	for h.Size < info.Size() {
		if err = ctx.Err(); err != nil {
			return fail(err)
		}
		line, e := readRecoveryLine(reader)
		if e != nil {
			return fail(fmt.Errorf("transcript record at byte %d: %w", h.Size, e))
		}
		offset := h.Size
		if len(bytes.TrimSpace(line)) > 0 {
			var r transcript.Record
			if !utf8.Valid(line) || json.Unmarshal(line, &r) != nil {
				return fail(fmt.Errorf("invalid transcript record at byte %d", offset))
			}
			if r.SessionID != "" && r.SessionID != j.id {
				return fail(fmt.Errorf("transcript session mismatch at byte %d", offset))
			}
			if r.Message != nil && r.Type != "boundary" {
				if llm.IsBoundaryMarker(*r.Message) {
					h.Boundary = true
					h.BoundaryOffset = offset
					h.BoundaryUsage = h.Usage
				}
				if e = j.indexControls(ctx, tx, *r.Message, offset); e != nil {
					return fail(e)
				}
				if r.Usage != nil {
					h.Usage.Add(*r.Usage)
				}
			}
			h.LastUUID = r.UUID
		}
		h.LastOffset = offset
		h.LastBytes = len(line)
		sum := sha256.Sum256(line)
		h.LastHash = hex.EncodeToString(sum[:])
		h.Size += int64(len(line))
		records++
		if records%128 == 0 {
			if err = commit(); err != nil {
				return fail(err)
			}
			tx, err = j.db.BeginTx(ctx, nil)
			if err != nil {
				return fail(err)
			}
		}
	}
	if err = commit(); err != nil {
		return fail(err)
	}
	// The source identity may have been replaced while an old open handle was
	// scanned. Never chain a new writer onto a different path's UUIDs.
	latest, err := os.Lstat(j.path)
	if err != nil || !os.SameFile(info, latest) {
		return fail(fmt.Errorf("transcript replaced while indexing"))
	}
	j.metrics.IndexedBytes = h.Size - start
	j.head = h
	return f, h, nil
}

func readRecoveryLine(r *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		part, err := r.ReadSlice('\n')
		if len(out)+len(part) > recoveryRecordBytes {
			return nil, fmt.Errorf("record exceeds byte limit")
		}
		out = append(out, part...)
		if err == nil {
			return out, nil
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == io.EOF {
			return nil, fmt.Errorf("incomplete final record")
		}
		return nil, err
	}
}

func (j *recoveryJournal) indexControls(ctx context.Context, tx *sql.Tx, m llm.Message, offset int64) error {
	if m.Role == llm.RoleUser {
		var writeErr error
		walkWorkerChatIDs(m.Text(), func(id string) bool {
			_, writeErr = tx.ExecContext(ctx, `INSERT INTO worker_requests(id,source_offset) VALUES(?,?) ON CONFLICT(id) DO NOTHING`, id, offset)
			return writeErr == nil
		})
		if writeErr != nil {
			return writeErr
		}
	}
	for _, b := range m.Content {
		if b.Type == llm.BlockToolUse && b.Name == "Skill" && b.ID != "" {
			var in struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(b.Input, &in) == nil && in.Name != "" {
				if _, err := tx.ExecContext(ctx, `INSERT INTO pending_skills(call_id,name,source_offset) VALUES(?,?,?) ON CONFLICT(call_id) DO UPDATE SET name=excluded.name,source_offset=excluded.source_offset`, b.ID, in.Name, offset); err != nil {
					return err
				}
			}
		}
		if b.Type == llm.BlockToolResult {
			if !b.IsError {
				if _, err := tx.ExecContext(ctx, `INSERT INTO skills(name,source_offset) SELECT name,source_offset FROM pending_skills WHERE call_id=? ON CONFLICT(name) DO NOTHING`, b.ToolUseID); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM pending_skills WHERE call_id=?`, b.ToolUseID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (j *recoveryJournal) Restore(ctx context.Context) ([]llm.Message, llm.Usage, string, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil, llm.Usage{}, "", fmt.Errorf("recovery journal closed")
	}
	f, h, err := j.syncSource(ctx)
	if err != nil {
		return nil, llm.Usage{}, "", err
	}
	if f == nil {
		return nil, llm.Usage{}, "", nil
	}
	defer f.Close()
	start := int64(0)
	if h.Boundary {
		start = h.BoundaryOffset
	}
	reader := bufio.NewReaderSize(io.NewSectionReader(f, start, h.Size-start), 64*1024)
	var messages []llm.Message
	for position := start; position < h.Size; {
		if err = ctx.Err(); err != nil {
			return nil, llm.Usage{}, "", err
		}
		line, e := readRecoveryLine(reader)
		if e != nil {
			return nil, llm.Usage{}, "", e
		}
		position += int64(len(line))
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var r transcript.Record
		if json.Unmarshal(line, &r) != nil {
			return nil, llm.Usage{}, "", fmt.Errorf("transcript changed during recovery")
		}
		if r.Type != "boundary" && r.Message != nil {
			messages = append(messages, *r.Message)
		}
	}
	if h.Boundary && (len(messages) < 3 || !llm.IsBoundaryMarker(messages[0]) || messages[1].Role != llm.RoleUser || messages[1].Text() == "" || hasRecoveryToolResult(messages[1])) {
		return nil, llm.Usage{}, "", fmt.Errorf("incomplete compacted working set")
	}
	for len(messages) > 0 && messages[len(messages)-1].Role == llm.RoleAssistant && len(messages[len(messages)-1].ToolUses()) > 0 {
		messages = messages[:len(messages)-1]
	}
	j.metrics.RestoredBytes = h.Size - start
	j.metrics.Messages = len(messages)
	return messages, h.Usage, h.LastUUID, nil
}

func hasRecoveryToolResult(m llm.Message) bool {
	for _, b := range m.Content {
		if b.Type == llm.BlockToolResult {
			return true
		}
	}
	return false
}

func (j *recoveryJournal) Sync(ctx context.Context) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return fmt.Errorf("recovery journal closed")
	}
	f, _, err := j.syncSource(ctx)
	if f != nil {
		f.Close()
	}
	return err
}

func (j *recoveryJournal) HasWorkerRequest(ctx context.Context, id string) (bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	var exists bool
	err := j.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM worker_requests WHERE id=?)`, id).Scan(&exists)
	return exists, err
}

func (j *recoveryJournal) ReplaySkills(ctx context.Context, unlock func(string)) error {
	if unlock == nil {
		return nil
	}
	offset, name := int64(-1), ""
	for {
		j.mu.Lock()
		rows, err := j.db.QueryContext(ctx, `SELECT source_offset,name FROM skills WHERE (source_offset,name)>(?,?) ORDER BY source_offset,name LIMIT 64`, offset, name)
		if err != nil {
			j.mu.Unlock()
			return err
		}
		var names []string
		for rows.Next() {
			if err = rows.Scan(&offset, &name); err != nil {
				break
			}
			names = append(names, name)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		j.mu.Unlock()
		if err != nil {
			return err
		}
		for _, n := range names {
			unlock(n)
		}
		if len(names) < 64 {
			return nil
		}
	}
}
