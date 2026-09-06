//go:build linux

package origin

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestRootedDirRootDescriptorIsBorrowedAndReturnedDescriptorsAreOwned(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "inside.txt")
	if err := os.WriteFile(inside, []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}

	rootFd, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(rootFd)
	rd := newRootedDir(rootFd, root)

	openedRoot, err := rd.openFile(".")
	if err != nil {
		t.Fatalf("open root as a file: %v", err)
	}
	info, err := openedRoot.Stat()
	if err != nil {
		openedRoot.Close()
		t.Fatal(err)
	}
	if !info.IsDir() {
		openedRoot.Close()
		t.Fatal("root descriptor did not identify a directory")
	}
	if err := openedRoot.Close(); err != nil {
		t.Fatal(err)
	}

	// Closing the descriptor returned for "." must not close the borrowed root.
	openedFile, err := rd.openFile("inside.txt")
	if err != nil {
		t.Fatalf("open after closing root view: %v", err)
	}
	defer openedFile.Close()
	data, err := os.ReadFile(inside)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "inside" {
		t.Fatalf("file contents = %q, want inside", data)
	}

	var stat unix.Stat_t
	if err := unix.Fstat(rootFd, &stat); err != nil {
		t.Fatalf("borrowed root fd was closed: %v", err)
	}
}

func TestRootedDirRejectsRegularFileAsRoot(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(filePath, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}

	rootFd, err := unix.Open(filePath, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(rootFd)

	rd := newRootedDir(rootFd, filePath)
	if _, err := rd.openDir("."); err == nil {
		t.Fatal("regular file was accepted as a rooted directory")
	}

	var stat unix.Stat_t
	if err := unix.Fstat(rootFd, &stat); err != nil {
		t.Fatalf("root fd was damaged by rejected open: %v", err)
	}
}
