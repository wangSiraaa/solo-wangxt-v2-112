//go:build linux

package backup

import (
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// openNoFollow opens a regular file without traversing a trailing symlink,
// closing the Lstat->open TOCTOU race (a swap to an outward symlink fails
// with ELOOP instead of reading outside the root).
func openNoFollow(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func uidOf(fi fs.FileInfo) int {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid)
	}
	return 0
}

func gidOf(fi fs.FileInfo) int {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Gid)
	}
	return 0
}

// sameIdentity is true when two stat results describe the same file object
// whose metadata has not moved: device+inode, size, mode and mtime.
func sameIdentity(a, b fs.FileInfo) bool {
	sa, oka := a.Sys().(*syscall.Stat_t)
	sb, okb := b.Sys().(*syscall.Stat_t)
	if !oka || !okb {
		return a.Size() == b.Size() && a.Mode() == b.Mode() && a.ModTime().Equal(b.ModTime())
	}
	return sa.Dev == sb.Dev && sa.Ino == sb.Ino &&
		sa.Size == sb.Size && sa.Mode == sb.Mode &&
		sa.Mtim == sb.Mtim
}

// bestEffortChown restores ownership; failure (typically unprivileged) is not
// fatal — mode and mtime are still preserved.
func bestEffortChown(path string, e Entry, follow bool) {
	_ = os.Lchown(path, e.UID, e.GID)
}

// applyLinkMeta sets ownership and timestamps on the symlink itself without
// following it (Lchown + utimensat AT_SYMLINK_NOFOLLOW).
func applyLinkMeta(path string, e Entry) {
	_ = os.Lchown(path, e.UID, e.GID)
	ts := []unix.Timespec{
		unix.NsecToTimespec(e.Mtime.UnixNano()),
		unix.NsecToTimespec(e.Mtime.UnixNano()),
	}
	_ = unix.UtimesNanoAt(unix.AT_FDCWD, path, ts, unix.AT_SYMLINK_NOFOLLOW)
	// Symlink permission bits are ignored on Linux, but callers that record
	// them on another platform still get a best-effort attempt.
	_ = os.Chmod(path, fs.FileMode(e.Mode)&0o7777)
}
