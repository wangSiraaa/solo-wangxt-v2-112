package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// BlobStore is a content-addressed store: blobs/<ab>/<64hex>.
// Chunks are addressed by the sha256 of their payload, so identical chunks
// from any snapshot share one file.
type BlobStore struct {
	root string
}

func NewBlobStore(root string) (*BlobStore, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return &BlobStore{root: root}, nil
}

func (b *BlobStore) path(id string) string {
	return filepath.Join(b.root, id[:2], id)
}

// Has reports whether a blob with the given id exists on disk with a size.
// Size zero is impossible for a stored blob; a 0-length file is treated as
// a torn write and reported missing.
func (b *BlobStore) Has(id string) bool {
	st, err := os.Stat(b.path(id))
	if err != nil || st.IsDir() || st.Size() == 0 {
		return false
	}
	return true
}

// Size returns the blob size.
func (b *BlobStore) Size(id string) (int64, error) {
	st, err := os.Stat(b.path(id))
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

var ErrBlobMissing = errors.New("blob missing")

// Open opens a blob for reading.
func (b *BlobStore) Open(id string) (*os.File, error) {
	if !b.Has(id) {
		return nil, fmt.Errorf("%w: %s", ErrBlobMissing, id)
	}
	return os.Open(b.path(id))
}

// Put writes data under the content id unless it already exists.
// The write goes through a temp file in the same directory + fsync + rename,
// so a crash never leaves a partial blob under its final name.
func (b *BlobStore) Put(id string, data []byte) error {
	final := b.path(id)
	if _, err := os.Stat(final); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(final), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o440); err != nil {
		return err
	}
	return os.Rename(tmpName, final)
}

// Verify re-hashes a blob and compares it to its content id and expected size.
func (b *BlobStore) Verify(id string, wantSize int64) error {
	if !b.Has(id) {
		return fmt.Errorf("%w: %s", ErrBlobMissing, id)
	}
	f, err := os.Open(b.path(id))
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	if n != wantSize {
		return fmt.Errorf("blob %s: length mismatch: manifest %d, store %d", id, wantSize, n)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != id {
		return fmt.Errorf("blob %s: digest mismatch: recomputed %s", id, got)
	}
	return nil
}
