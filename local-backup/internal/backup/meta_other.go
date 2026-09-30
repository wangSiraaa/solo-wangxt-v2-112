//go:build !linux

package backup

import (
	"io/fs"
	"os"
)

func uidOf(fi fs.FileInfo) int { return 0 }
func gidOf(fi fs.FileInfo) int { return 0 }

func sameIdentity(a, b fs.FileInfo) bool {
	return a.Size() == b.Size() && a.Mode() == b.Mode() && a.ModTime().Equal(b.ModTime())
}

func bestEffortChown(path string, e Entry, follow bool) {}

func openNoFollow(path string) (*os.File, error) {
	// Best effort: WalkDir already Lstats and we re-stat after reading, so a
	// swap is detected; only Linux gets the hard O_NOFOLLOW guarantee.
	return os.Open(path)
}

func applyLinkMeta(path string, e Entry) {
	_ = os.Chtimes(path, e.Mtime, e.Mtime)
}
