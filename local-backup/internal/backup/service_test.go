package backup

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func newTestService(t *testing.T, fault FaultInjection) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	svc, err := NewService(filepath.Join(dir, "repo"), fault)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(func() { svc.Close() })
	return svc, dir
}

// Deterministic pseudo-random content: large enough to produce several chunks
// at the restic defaults (~1 MiB average).
func fillFile(t *testing.T, p string, n int64, seed byte) {
	t.Helper()
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, 64*1024)
	var written int64
	for written < n {
		for i := range buf {
			buf[i] = seed + byte(int64(i)*7+written/int64(len(buf)))
		}
		c := int64(len(buf))
		if n-written < c {
			c = n - written
		}
		if _, err := f.Write(buf[:c]); err != nil {
			t.Fatal(err)
		}
		written += c
	}
}

func TestBackupRestoreRoundTrip(t *testing.T) {
	svc, dir := newTestService(t, FaultInjection{})
	root := filepath.Join(dir, "data")
	os.MkdirAll(filepath.Join(root, "sub"), 0o755)

	fillFile(t, filepath.Join(root, "big.bin"), 4*1024*1024, 1)
	os.WriteFile(filepath.Join(root, "sub", "note.txt"), []byte("hello\n"), 0o644)
	os.WriteFile(filepath.Join(root, "empty.dat"), nil, 0o644)
	if err := os.Symlink("sub/note.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}

	r, err := svc.Backup(root)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if !r.Complete {
		t.Fatalf("snapshot not complete: %s", r.Reason)
	}
	if r.NewChunks == 0 {
		t.Fatalf("expected new chunks for 4MiB file")
	}
	if _, err := os.Stat(filepath.Join(root, "empty.dat")); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(dir, "restored")
	rr, err := svc.Restore(r.SnapshotID, target)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !rr.Verified {
		t.Fatalf("restore not verified: %v", rr.Errors)
	}

	// Empty file restored with zero length.
	if st, err := os.Stat(filepath.Join(target, "empty.dat")); err != nil || st.Size() != 0 {
		t.Fatalf("empty file restore wrong: %v size=%v", err, st)
	}
	// Symlink target preserved as a link (not followed/duplicated).
	tgt, err := os.Readlink(filepath.Join(target, "link"))
	if err != nil || tgt != "sub/note.txt" {
		t.Fatalf("symlink not preserved: %q %v", tgt, err)
	}
	// Directory mode preserved.
	st, _ := os.Stat(filepath.Join(target, "sub"))
	if st.Mode().Perm() != 0o750 {
		t.Fatalf("dir perm = %o, want 750", st.Mode().Perm())
	}
}

func TestIncrementalChunkReuse(t *testing.T) {
	svc, dir := newTestService(t, FaultInjection{})
	root := filepath.Join(dir, "data")
	os.MkdirAll(root, 0o755)
	p := filepath.Join(root, "growing.bin")
	fillFile(t, p, 20*1024*1024, 3)

	r1, err := svc.Backup(root)
	if err != nil {
		t.Fatal(err)
	}
	if !r1.Complete {
		t.Fatalf("first snapshot: %s", r1.Reason)
	}
	if r1.Reused != 0 {
		t.Fatalf("first backup should reuse nothing, got %d", r1.Reused)
	}
	if r1.NewChunks < 3 {
		t.Fatalf("test needs a multi-chunk file, got %d chunks", r1.NewChunks)
	}

	// Small change: overwrite a region in the middle (append would shift only
	// later chunk boundaries; overwrite keeps preceding boundaries identical).
	f, _ := os.OpenFile(p, os.O_WRONLY, 0o644)
	if _, err := f.WriteAt([]byte(strings.Repeat("X", 4096)), 10*1024*1024); err != nil {
		t.Fatal(err)
	}
	f.Close()

	r2, err := svc.Backup(root)
	if err != nil {
		t.Fatal(err)
	}
	if !r2.Complete {
		t.Fatalf("second snapshot: %s", r2.Reason)
	}
	if r2.Reused == 0 {
		t.Fatalf("expected most chunks to be reused after small edit, new=%d reused=%d",
			r2.NewChunks, r2.Reused)
	}
	total := r2.NewChunks + r2.Reused
	if reusedFrac := float64(r2.Reused) / float64(total); reusedFrac < 0.5 {
		t.Fatalf("reuse fraction too low: %.2f (new=%d reused=%d)", reusedFrac, r2.NewChunks, r2.Reused)
	}
	t.Logf("chunk reuse after 4KiB edit in 4MiB: new=%d reused=%d", r2.NewChunks, r2.Reused)

	// Restore second snapshot and compare digest implicitly via verification.
	target := filepath.Join(dir, "restored2")
	rr, err := svc.Restore(r2.SnapshotID, target)
	if err != nil || !rr.Verified {
		t.Fatalf("restore: %v %v", err, rr)
	}
}

func TestSkipChunkFaultIsDetected(t *testing.T) {
	svc, dir := newTestService(t, FaultInjection{SkipNewChunk: 2})
	root := filepath.Join(dir, "data")
	os.MkdirAll(root, 0o755)
	fillFile(t, filepath.Join(root, "big.bin"), 20*1024*1024, 7)

	r, err := svc.Backup(root)
	if err != nil {
		t.Fatal(err)
	}
	if r.Complete {
		t.Fatal("snapshot should be incomplete with missing chunk")
	}
	if len(r.MissingChunks) == 0 {
		t.Fatal("expected missing chunks in report")
	}

	// Diag names the exact chunk and the affected entry.
	_, missing, err := svc.Diagnose(r.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 {
		t.Fatalf("want 1 missing chunk group, got %d", len(missing))
	}
	if len(missing[0].Entries) != 1 || missing[0].Entries[0] != "big.bin" {
		t.Fatalf("missing chunk should map to big.bin, got %#v", missing)
	}

	// Restore must be refused, and no target created.
	target := filepath.Join(dir, "nope")
	_, err = svc.Restore(r.SnapshotID, target)
	if err == nil {
		t.Fatal("restore of incomplete snapshot should fail")
	}
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("target must not be created on refused restore: %v", statErr)
	}
}

func TestInterruptedSnapshot(t *testing.T) {
	name := t.Name()

	if os.Getenv("CRASH_TEST") == name {
		// Child process: reproduce a crash mid-commit.
		dir := os.Getenv("CRASH_DIR")
		root := filepath.Join(dir, "data")
		svc, err := NewService(filepath.Join(dir, "repo"), FaultInjection{CrashCommit: true})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = svc.Backup(root)
		return
	}

	dir := t.TempDir()
	root := filepath.Join(dir, "data")
	os.MkdirAll(root, 0o755)
	fillFile(t, filepath.Join(root, "big.bin"), 2*1024*1024, 9)

	cmd := newSelfCmd(t, name)
	cmd.Env = append(cmd.Env, "CRASH_DIR="+dir)
	if err := cmd.Run(); err == nil {
		t.Fatal("fault should have terminated the process with an error")
	}

	svc2, err := NewService(filepath.Join(dir, "repo"), FaultInjection{})
	if err != nil {
		t.Fatal(err)
	}
	defer svc2.Close()
	snaps, err := svc2.manifest.ListSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 || snaps[0].State != StatusInterrupted {
		t.Fatalf("expected one interrupted snapshot, got %#v", snaps)
	}
	snap, missing, err := svc2.Diagnose(snaps[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if snap.State != StatusInterrupted {
		t.Fatalf("state = %s", snap.State)
	}
	if len(missing) != 0 {
		t.Fatalf("blobs were flushed per entry; want 0 missing, got %#v", missing)
	}
}

func TestFileMutatedDuringScan(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix semantics")
	}
	svc, dir := newTestService(t, FaultInjection{})
	root := filepath.Join(dir, "data")
	os.MkdirAll(root, 0o755)
	p := filepath.Join(root, "mutating.log")
	fillFile(t, p, 3*1024*1024, 2)

	// Keep appending in a tight loop for the whole backup so the file can
	// never settle between the two read attempts.
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return
		}
		defer f.Close()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
				f.WriteString(strings.Repeat("z", 256*1024))
				f.Sync()
				i++
			}
		}
	}()
	// Give the goroutine a moment and run the backup while it writes.
	time.Sleep(100 * time.Millisecond)
	r, err := svc.Backup(root)
	close(stop)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if r.Complete {
		t.Fatal("snapshot with a mutating file must not be complete")
	}
	found := false
	for _, c := range r.ChangedEntries {
		if filepath.Base(c) == "mutating.log" {
			found = true
		}
	}
	if !found {
		t.Fatalf("mutating.log should be listed as changed: %v", r.ChangedEntries)
	}
}

func TestRestoreRefusesNonEmptyTargetAndSymlinkEscape(t *testing.T) {
	svc, dir := newTestService(t, FaultInjection{})
	root := filepath.Join(dir, "data")
	os.MkdirAll(root, 0o755)
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("A"), 0o644)
	os.Symlink("/etc/passwd", filepath.Join(root, "evil"))

	r, err := svc.Backup(root)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Complete {
		t.Fatal(r.Reason)
	}

	target := filepath.Join(dir, "t")
	os.MkdirAll(target, 0o755)
	os.WriteFile(filepath.Join(target, "occupant"), []byte("x"), 0o644)
	if _, err := svc.Restore(r.SnapshotID, target); err == nil {
		t.Fatal("restore into non-empty dir must be refused")
	}
	os.RemoveAll(target)

	rr, err := svc.Restore(r.SnapshotID, target)
	if err != nil || !rr.Verified {
		t.Fatalf("restore: %v %v", err, rr)
	}
	// The symlink is restored as a link pointing outward, but nothing was
	// read or written through it during backup or restore.
	tgt, err := os.Readlink(filepath.Join(target, "evil"))
	if err != nil || tgt != "/etc/passwd" {
		t.Fatalf("link target: %q %v", tgt, err)
	}
}
