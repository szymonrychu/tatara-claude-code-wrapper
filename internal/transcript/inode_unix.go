//go:build !windows

package transcript

import (
	"os"
	"syscall"
)

func inode(fi os.FileInfo) uint64 {
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		return sys.Ino
	}
	return 0
}
