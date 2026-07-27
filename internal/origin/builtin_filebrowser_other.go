//go:build !linux

package origin

import "os"

// renameNoReplaceImpl renames src to dst using the standard syscall on
// non-Linux platforms. Silent overwrites are possible on these systems.
func renameNoReplaceImpl(src, dst string) error {
	return os.Rename(src, dst)
}
