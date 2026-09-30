package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/restic/chunker"
)

// FaultInjection is parsed from the FAULT_INJECT environment variable and is
// only used by the demo to prove that incomplete snapshots are diagnosable.
//
//	skip-chunk=N  silently skip writing the blob for the Nth *new* chunk
//	crash-commit  exit the process right before the snapshot is finalized
type FaultInjection struct {
	SkipNewChunk int // 1-based index, 0 = off
	CrashCommit  bool
}

// Service is the backup engine. One backup/restore runs at a time.
type Service struct {
	repoDir  string
	manifest *Manifest
	blobs    *BlobStore
	poly     chunker.Pol
	fault    FaultInjection

	mu sync.Mutex
}

func NewService(repoDir string, fault FaultInjection) (*Service, error) {
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		return nil, err
	}
	mf, err := OpenManifest(filepath.Join(repoDir, "manifest.db"))
	if err != nil {
		return nil, err
	}
	bs, err := NewBlobStore(filepath.Join(repoDir, "blobs"))
	if err != nil {
		mf.Close()
		return nil, err
	}
	poly, err := mf.Poly()
	if err != nil {
		mf.Close()
		return nil, err
	}
	if poly == 0 {
		p, err := chunker.RandomPolynomial()
		if err != nil {
			mf.Close()
			return nil, err
		}
		poly = uint64(p)
		if err := mf.SetPoly(poly); err != nil {
			mf.Close()
			return nil, err
		}
	}
	return &Service{
		repoDir: repoDir, manifest: mf, blobs: bs,
		poly: chunker.Pol(poly), fault: fault,
	}, nil
}

func (s *Service) Close() error { return s.manifest.Close() }

type plannedChunk struct {
	id   string
	data []byte
	off  int64
}

// Backup walks root and records an incremental snapshot.
func (s *Service) Backup(root string) (*BackupReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	absRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(absRoot)
	if err != nil {
		return nil, fmt.Errorf("root not accessible: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("backup root must be a directory: %s", absRoot)
	}

	snapID, err := s.manifest.StartSnapshot(absRoot)
	if err != nil {
		return nil, err
	}

	r := &BackupReport{SnapshotID: snapID}
	newChunkCounter := 0 // counts only chunks genuinely new to the whole store

	// walk does not follow symlinks (WalkDir uses Lstat).
	walkErr := filepath.WalkDir(absRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			r.ChangedEntries = append(r.ChangedEntries, relOrDie(absRoot, path))
			return nil
		}
		rel, err := safeRel(absRoot, path)
		if err != nil {
			return err
		}

		fi, err := d.Info()
		if err != nil {
			r.ChangedEntries = append(r.ChangedEntries, rel)
			return nil
		}

		mode := fi.Mode()
		switch {
		case mode.IsDir():
			if err := s.storeEntry(snapID, rel, KindDir, fi, nil, "", EntryValid); err != nil {
				return err
			}
			r.Dirs++
		case mode&fs.ModeSymlink != 0:
			// Store the link itself; never read through it.
			target, err := os.Readlink(path)
			if err != nil {
				r.ChangedEntries = append(r.ChangedEntries, rel)
				return nil
			}
			if err := s.storeEntry(snapID, rel, KindLink, fi, nil, target, EntryValid); err != nil {
				return err
			}
			r.Links++
		case mode.IsRegular():
			entryState, chunks, fileSum, err := s.readRegular(absRoot, rel, path, fi, snapID, &newChunkCounter)
			if err != nil {
				r.ChangedEntries = append(r.ChangedEntries, rel)
				return nil
			}
			if entryState != EntryValid {
				// Mutated while scanning: mark unfinished, store no chunks.
				if err := s.storeEntry(snapID, rel, KindFile, fi, nil, "", entryState); err != nil {
					return err
				}
				r.Files++
				r.ChangedEntries = append(r.ChangedEntries, rel)
				return nil
			}
			if err := s.commitFile(snapID, rel, fi, chunks, fileSum, &newChunkCounter, r); err != nil {
				return err
			}
			r.Files++
			r.Bytes += fi.Size()
		default:
			// fifo / socket / device: recorded but not backed up.
			if err := s.storeEntry(snapID, rel, KindOther, fi, nil, "", EntryUnsupported); err != nil {
				return err
			}
			r.UnsupportedEntries = append(r.UnsupportedEntries, rel)
		}
		return nil
	})
	if walkErr != nil {
		_ = s.manifest.FinalizeSnapshot(snapID, StatusIncomplete,
			"walk error: "+walkErr.Error(), r.Files, r.Dirs, r.Links, r.Bytes)
		return nil, walkErr
	}

	if err := s.manifest.UpdateCounters(snapID, r.Files, r.Dirs, r.Links, r.Bytes); err != nil {
		return nil, err
	}

	// Demo fault: die with an interrupted row; chunks written so far are on disk.
	if s.fault.CrashCommit {
		os.Exit(99)
	}

	// ---- Verification gate -------------------------------------------------
	// A snapshot is complete only if every distinct chunk it references is
	// present in the blob store and re-hashes to its content id.
	ids, err := s.manifest.SnapshotChunkIDs(snapID)
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, id := range ids {
		wantSize, err := s.chunkSize(id)
		if err != nil {
			missing = append(missing, id)
			continue
		}
		if err := s.blobs.Verify(id, wantSize); err != nil {
			missing = append(missing, id)
		}
	}
	r.MissingChunks = missing

	state := StatusComplete
	var reasons []string
	if len(missing) > 0 {
		state = StatusIncomplete
		reasons = append(reasons, fmt.Sprintf("%d missing/corrupt content chunk(s)", len(missing)))
	}
	if len(r.ChangedEntries) > 0 {
		state = StatusIncomplete
		reasons = append(reasons, fmt.Sprintf("%d entry/entries changed during scan", len(r.ChangedEntries)))
	}
	if len(r.UnsupportedEntries) > 0 {
		state = StatusIncomplete
		reasons = append(reasons, fmt.Sprintf("%d unsupported entry/entries", len(r.UnsupportedEntries)))
	}
	reason := strings.Join(reasons, "; ")
	r.Complete = state == StatusComplete
	r.Reason = reason

	if err := s.manifest.FinalizeSnapshot(snapID, state, reason,
		r.Files, r.Dirs, r.Links, r.Bytes); err != nil {
		return nil, err
	}
	return r, nil
}

// readRegular chunks one file, retrying once if size/mtime/inode moved while
// it was being read. Empty files produce no chunks and a digest of the empty
// input, which restore verifies just like any other file.
func (s *Service) readRegular(root, rel, path string, fi fs.FileInfo, snapID int64, newCounter *int) (string, []plannedChunk, []byte, error) {
	const maxAttempts = 2
	var chunks []plannedChunk
	var fileSum []byte
	var readSize int64
	var before fs.FileInfo

	for attempt := 0; attempt < maxAttempts; attempt++ {
		var err error
		before, err = os.Lstat(path)
		if err != nil {
			return EntryChanged, nil, nil, err
		}
		if !before.Mode().IsRegular() {
			// Replaced by a symlink/special file between walk and open.
			return EntryChanged, nil, nil, errors.New("no longer a regular file")
		}

		chunks = chunks[:0]
		readSize = 0
		f, err := openNoFollow(path)
		if err != nil {
			return EntryChanged, nil, nil, err
		}
		ck := chunker.New(f, s.poly)
		fileHasher := sha256.New()

		buf := make([]byte, 0)
		readErr := error(nil)
		for {
			var c chunker.Chunk
			c, readErr = ck.Next(buf)
			if readErr == io.EOF {
				readErr = nil
				break
			}
			if readErr != nil {
				break
			}
			sum := sha256.Sum256(c.Data)
			id := hex.EncodeToString(sum[:])
			// chunker reuses the backing array of the buffer passed to Next,
			// so copy before retaining the payload.
			payload := make([]byte, len(c.Data))
			copy(payload, c.Data)
			chunks = append(chunks, plannedChunk{id: id, data: payload, off: int64(c.Start)})
			fileHasher.Write(payload)
			readSize += int64(len(c.Data))
			buf = c.Data[:0]
		}
		f.Close()

		after, statErr := os.Lstat(path)
		if statErr != nil {
			return EntryChanged, nil, nil, statErr
		}
		stable := readErr == nil &&
			sameIdentity(before, after) &&
			after.Size() == readSize

		if stable {
			fileSum = fileHasher.Sum(nil)
			return EntryValid, chunks, fileSum, nil
		}
		// Changed while reading: loop once more and re-read from scratch.
	}
	return EntryChanged, nil, nil, errors.New("file modified during scan; remains unstable after re-read")
}

// commitFile records one stable file: its entry, chunk rows and blob bytes.
func (s *Service) commitFile(snapID int64, rel string, fi fs.FileInfo, chunks []plannedChunk, fileSum []byte, newCounter *int, r *BackupReport) error {
	tx, err := s.manifest.begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	entryID, err := s.manifest.InsertEntry(tx, &Entry{
		SnapshotID: snapID, RelPath: rel, Kind: KindFile,
		Mode: modeBits(fi), UID: uidOf(fi), GID: gidOf(fi),
		Mtime: fi.ModTime(), Size: fi.Size(), Sha256: fileSum, State: EntryValid,
	})
	if err != nil {
		return err
	}

	seen := map[string]bool{}
	for seq, pc := range chunks {
		known, err := s.manifest.ChunkKnown(pc.id)
		if err != nil {
			return err
		}
		if !known {
			*newCounter++
			if !(s.fault.SkipNewChunk > 0 && *newCounter == s.fault.SkipNewChunk) {
				if err := s.blobs.Put(pc.id, pc.data); err != nil {
					return err
				}
			} // else: simulated lost blob — manifest still references it below
			if err := s.manifest.InsertChunk(tx, pc.id, int64(len(pc.data)), snapID); err != nil {
				return err
			}
			r.NewChunks++
		} else {
			r.Reused++
		}
		if err := s.manifest.InsertFileChunk(tx, entryID, seq,
			ChunkRef{EntryID: entryID, ChunkID: pc.id, Offset: pc.off, Length: int64(len(pc.data))}); err != nil {
			return err
		}
		if !seen[pc.id] {
			seen[pc.id] = true
			if err := s.manifest.LinkSnapshotChunk(tx, snapID, pc.id); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *Service) storeEntry(snapID int64, rel, kind string, fi fs.FileInfo, sum []byte, target, state string) error {
	tx, err := s.manifest.begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = s.manifest.InsertEntry(tx, &Entry{
		SnapshotID: snapID, RelPath: rel, Kind: kind,
		Mode: modeBits(fi), UID: uidOf(fi), GID: gidOf(fi),
		Mtime: fi.ModTime(), Size: fi.Size(), Sha256: sum,
		LinkTarget: target, State: state,
	})
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) chunkSize(id string) (int64, error) {
	var n int64
	err := s.manifest.dbQuerySize(id, &n)
	return n, err
}

// Diagnose re-checks every chunk a snapshot references against the blob store.
func (s *Service) Diagnose(snapshotID int64) (*Snapshot, []MissingChunk, error) {
	snap, err := s.manifest.GetSnapshot(snapshotID)
	if err != nil {
		return nil, nil, err
	}
	missing, err := s.manifest.MissingChunksForSnapshot(snapshotID, s.blobs.Has)
	if err != nil {
		return nil, nil, err
	}
	// Also report blobs that exist but fail to re-hash.
	for _, id := range s.must(s.manifest.SnapshotChunkIDs(snapshotID)) {
		var size int64
		if err := s.manifest.dbQuerySize(id, &size); err != nil {
			continue
		}
		if s.blobs.Has(id) && s.blobs.Verify(id, size) != nil {
			entries := s.entriesUsingChunk(snapshotID, id)
			missing = append(missing, MissingChunk{ChunkID: id + " (corrupt)", Entries: entries})
		}
	}
	return snap, missing, nil
}

func (s *Service) must(ids []string, err error) []string {
	if err != nil {
		return nil
	}
	return ids
}

func (s *Service) entriesUsingChunk(snapID int64, chunkID string) []string {
	rows, err := s.manifest.db.Query(
		`SELECT e.relpath FROM file_chunks fc
		 JOIN entries e ON e.id=fc.entry_id
		 WHERE e.snapshot_id=? AND fc.chunk_id=? ORDER BY e.relpath`, snapID, chunkID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			out = append(out, p)
		}
	}
	return out
}

// ---- restore ---------------------------------------------------------------

// Restore recreates a snapshot inside target. target must not exist or must
// be an empty directory; nothing that already exists there is overwritten.
func (s *Service) Restore(snapshotID int64, target string) (*RestoreReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	snap, err := s.manifest.GetSnapshot(snapshotID)
	if err != nil {
		return nil, err
	}
	if snap.State != StatusComplete {
		return nil, fmt.Errorf("snapshot %d is %s (%s); run diagnosis before any restore",
			snapshotID, snap.State, snap.Reason)
	}
	entries, err := s.manifest.EntriesOf(snapshotID)
	if err != nil {
		return nil, err
	}

	absTarget, err := filepath.Abs(filepath.Clean(target))
	if err != nil {
		return nil, err
	}

	// Preflight 1: destination must be absent or empty.
	if st, err := os.Lstat(absTarget); err == nil {
		if !st.IsDir() {
			return nil, fmt.Errorf("restore target exists and is not a directory: %s", absTarget)
		}
		f, err := os.ReadDir(absTarget)
		if err != nil {
			return nil, err
		}
		if len(f) > 0 {
			return nil, fmt.Errorf("restore target is not empty, refusing to overwrite: %s", absTarget)
		}
	}

	// Preflight 2: every chunk of every valid file must verify. A failed
	// snapshot cannot be silently half-restored.
	var missing []MissingChunk
	validEntries := 0
	for _, e := range entries {
		if e.Kind != KindFile || e.State != EntryValid {
			continue
		}
		validEntries++
		refs, err := s.manifest.FileChunksOf(e.ID)
		if err != nil {
			return nil, err
		}
		for _, ref := range refs {
			size, err := s.manifest.chunkSizeQ(ref.ChunkID)
			if err != nil || s.blobs.Verify(ref.ChunkID, size) != nil {
				missing = appendMissing(missing, ref.ChunkID, e.RelPath)
			}
		}
	}
	if len(missing) > 0 {
		return nil, &RestoreUnsafeError{SnapshotID: snapshotID, Missing: missing}
	}

	if err := os.MkdirAll(absTarget, 0o700); err != nil {
		return nil, err
	}

	rep := &RestoreReport{SnapshotID: snapshotID, Target: absTarget}

	// Pass 1: directories (paths sort before their children).
	var dirs []Entry
	for _, e := range entries {
		if e.Kind == KindDir {
			dirs = append(dirs, e)
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].RelPath < dirs[j].RelPath })
	for _, d := range dirs {
		p := joinUnder(absTarget, d.RelPath)
		if d.RelPath == "." {
			p = absTarget
		}
		if err := os.Mkdir(p, fs.FileMode(d.Mode)&0o777|0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("mkdir %s: %w", d.RelPath, err)
		}
		rep.Dirs++
	}

	// Pass 2: files and symlinks, verifying digest and length per file.
	sort.Slice(entries, func(i, j int) bool { return entries[i].RelPath < entries[j].RelPath })
	for _, e := range entries {
		if e.RelPath != "." {
			// Defend against a tampered manifest: an entry name must not
			// resolve outside the restore target.
			probe := joinUnder(absTarget, e.RelPath)
			if !insideRoot(absTarget, probe) {
				rep.Errors = append(rep.Errors, e.RelPath+": entry path escapes restore target, skipped")
				continue
			}
		}
		p := joinUnder(absTarget, e.RelPath)
		switch e.Kind {
		case KindFile:
			if e.State != EntryValid {
				continue // incomplete entries are not restored
			}
			if err := s.restoreFile(p, e); err != nil {
				rep.Errors = append(rep.Errors, e.RelPath+": "+err.Error())
				continue
			}
			rep.Files++
		case KindLink:
			if err := os.Symlink(e.LinkTarget, p); err != nil {
				// Symlink already exists: refuse to overwrite.
				rep.Errors = append(rep.Errors, e.RelPath+": "+err.Error())
				continue
			}
			rep.Links++
			applyLinkMeta(p, e)
		}
	}

	// Pass 3: finalize directory metadata deepest-first so writes above never
	// lock us out, then set mtimes deepest-first.
	for i := len(dirs) - 1; i >= 0; i-- {
		d := dirs[i]
		p := absTarget
		if d.RelPath != "." {
			p = joinUnder(absTarget, d.RelPath)
		}
		_ = os.Chmod(p, fs.FileMode(d.Mode)&0o7777)
		bestEffortChown(p, d, false)
		_ = os.Chtimes(p, d.Mtime, d.Mtime)
	}

	rep.Verified = len(rep.Errors) == 0 && validEntries == rep.Files
	if !rep.Verified && len(rep.Errors) == 0 {
		rep.Errors = append(rep.Errors, "restored file count does not match manifest")
	}
	return rep, nil
}

func (s *Service) restoreFile(p string, e Entry) error {
	refs, err := s.manifest.FileChunksOf(e.ID)
	if err != nil {
		return err
	}
	// O_EXCL: never overwrite an existing file.
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	var n int64
	for _, ref := range refs {
		bf, err := s.blobs.Open(ref.ChunkID)
		if err != nil {
			f.Close()
			return err
		}
		wn, werr := io.Copy(io.MultiWriter(f, h), io.LimitReader(bf, ref.Length))
		bf.Close()
		if werr != nil {
			f.Close()
			return werr
		}
		if wn != ref.Length {
			f.Close()
			return fmt.Errorf("chunk %s truncated: want %d, got %d", ref.ChunkID, ref.Length, wn)
		}
		n += wn
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	if n != e.Size {
		return fmt.Errorf("length mismatch: manifest %d, restored %d", e.Size, n)
	}
	if e.Sha256 != nil {
		got := h.Sum(nil)
		if !equalBytes(got, e.Sha256) {
			return fmt.Errorf("digest mismatch: manifest %x, restored %x", e.Sha256, got)
		}
	}
	if err := os.Chmod(p, fs.FileMode(e.Mode)&0o7777); err != nil {
		return err
	}
	bestEffortChown(p, e, false)
	return os.Chtimes(p, e.Mtime, e.Mtime)
}

// RestoreUnsafeError is returned when preflight finds missing chunks.
type RestoreUnsafeError struct {
	SnapshotID int64
	Missing    []MissingChunk
}

func (e *RestoreUnsafeError) Error() string {
	return fmt.Sprintf("snapshot %d cannot be restored: %d chunk set(s) missing",
		e.SnapshotID, len(e.Missing))
}

// ---- helpers ---------------------------------------------------------------

func appendMissing(list []MissingChunk, id, rel string) []MissingChunk {
	for i := range list {
		if list[i].ChunkID == id {
			list[i].Entries = append(list[i].Entries, rel)
			return list
		}
	}
	return append(list, MissingChunk{ChunkID: id, Entries: []string{rel}})
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func safeRel(root, path string) (string, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", err
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) {
		return "", fmt.Errorf("path escapes backup root: %s", path)
	}
	return rel, nil
}

// joinUnder joins rel under root and guarantees the result stays inside root.
func joinUnder(root, rel string) string {
	clean := filepath.Clean(filepath.Join("/", filepath.FromSlash(rel)))
	return filepath.Join(root, clean)
}

// insideRoot reports whether path is root itself or below it.
func insideRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

func relOrDie(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}
