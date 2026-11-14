//go:build unix

package traffic

import (
	"os"
	"syscall"
)

// fileID identifies a file across renames, so log rotation is noticed.
func fileID(st os.FileInfo) uint64 {
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		return uint64(s.Ino)
	}
	return 0
}
