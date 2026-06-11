//go:build windows

package transcript

import "os"

func inode(fi os.FileInfo) uint64 {
	return 0
}
