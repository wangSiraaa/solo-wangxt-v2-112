package backup

import (
	"io/fs"
	"time"
)

// Snapshot states. A snapshot is only "complete" after every referenced
// content chunk has been verified present in the blob store.
const (
	StatusComplete    = "complete"    // every entry valid, every chunk verified
	StatusIncomplete  = "incomplete"  // backup ran but verification found problems
	StatusInterrupted = "interrupted" // process died before finalization
)

// Entry states recorded per path inside a snapshot.
const (
	EntryValid       = "valid"       // hashed and chunked consistently
	EntryChanged     = "changed"     // file mutated during scan even after re-read
	EntryUnsupported = "unsupported" // non-regular file (socket/device/fifo...)
)

// Kind of filesystem entry.
const (
	KindFile  = "file"
	KindDir   = "dir"
	KindLink  = "symlink"
	KindOther = "other"
)

// modeBits stores permission bits plus setuid/setgid/sticky.
func modeBits(fi fs.FileInfo) uint32 {
	return uint32(fi.Mode().Perm() | fi.Mode()&fs.ModeSetuid | fi.Mode()&fs.ModeSetgid | fi.Mode()&fs.ModeSticky)
}

// Snapshot describes one backup run's row in the manifest.
type Snapshot struct {
	ID         int64      `json:"id"`
	Root       string     `json:"root"`
	State      string     `json:"state"`
	Reason     string     `json:"reason"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at"`
	Files      int        `json:"files"`
	Dirs       int        `json:"dirs"`
	Links      int        `json:"links"`
	Bytes      int64      `json:"bytes"`
}

// Entry is one filesystem object inside a snapshot.
type Entry struct {
	ID         int64     `json:"id"`
	SnapshotID int64     `json:"snapshot_id"`
	RelPath    string    `json:"relpath"`
	Kind       string    `json:"kind"`
	Mode       uint32    `json:"mode"`
	UID        int       `json:"uid"`
	GID        int       `json:"gid"`
	Mtime      time.Time `json:"mtime"`
	Size       int64     `json:"size"`
	Sha256     []byte    `json:"sha256"`
	LinkTarget string    `json:"link_target"`
	State      string    `json:"state"`
}

// ChunkRef is one chunk position within a file inside a snapshot.
type ChunkRef struct {
	EntryID int64
	ChunkID string // sha256 hex of chunk content
	Offset  int64
	Length  int64
}

// MissingChunk pairs a referenced chunk id with the entries that need it.
type MissingChunk struct {
	ChunkID string   `json:"chunk_id"`
	Entries []string `json:"entries"`
}

// BackupReport is returned by Create.
type BackupReport struct {
	SnapshotID int64  `json:"snapshot_id"`
	Complete   bool   `json:"complete"`
	Reason     string `json:"reason,omitempty"`

	Files     int   `json:"files"`
	Dirs      int   `json:"dirs"`
	Links     int   `json:"links"`
	Bytes     int64 `json:"bytes"`
	NewChunks int   `json:"new_chunks"`
	Reused    int   `json:"chunks_reused"`

	ChangedEntries     []string `json:"changed_entries,omitempty"`
	UnsupportedEntries []string `json:"unsupported_entries,omitempty"`
	MissingChunks      []string `json:"missing_chunks,omitempty"`
}

// RestoreReport is returned by Restore.
type RestoreReport struct {
	SnapshotID int64    `json:"snapshot_id"`
	Target     string   `json:"target"`
	Files      int      `json:"files"`
	Dirs       int      `json:"dirs"`
	Links      int      `json:"links"`
	Verified   bool     `json:"verified"` // all digests and lengths matched
	Errors     []string `json:"errors,omitempty"`
}
