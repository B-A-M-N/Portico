package origin

import "golang.org/x/sys/unix"

// renameNoReplaceImpl renames src to dst using RENAME_NOREPLACE to prevent
// silent overwrites of existing files.
func renameNoReplaceImpl(src, dst string) error {
	return unix.Renameat2(unix.AT_FDCWD, src, unix.AT_FDCWD, dst, unix.RENAME_NOREPLACE)
}
