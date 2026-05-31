package storage

import (
	"fmt"
	"syscall"
)

// DiskStats describes the actual on-disk usage for a mount point.
// Bytes only — translate to GB/TB at the presentation layer.
type DiskStats struct {
	Path  string `json:"path"`
	Total uint64 `json:"total"`  // total bytes on the filesystem holding Path
	Free  uint64 `json:"free"`   // bytes available to non-root processes
	Used  uint64 `json:"used"`   // Total - Free
}

// Stat queries the filesystem via statfs(2) and returns capacity in bytes.
// Works on Linux + macOS (both have syscall.Statfs_t).
//
// Important: we use Bavail (free to non-root) not Bfree (truly free including
// the ~5% reserved root blocks on ext4). This is what `df` shows in the
// "Available" column and matches what an unprivileged container can write.
func Stat(path string) (*DiskStats, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return nil, fmt.Errorf("statfs %s: %w", path, err)
	}
	bsize := uint64(fs.Bsize)
	total := fs.Blocks * bsize
	free := fs.Bavail * bsize
	if free > total {
		free = total
	}
	return &DiskStats{
		Path:  path,
		Total: total,
		Free:  free,
		Used:  total - free,
	}, nil
}
