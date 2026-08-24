package cloudflare

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A credential file must not outlive the process that reads it.
//
// The connector is started with a path to a file containing a Cloudflare tunnel
// token. It is a real secret, briefly on disk, and every way the start can fail is a
// way for it to be left there. The audit names creation, readiness and cleanup
// failure paths as uncovered, and they are the paths where a lingering token is the
// consequence.
//
// The happy path was covered. What follows is every unhappy one.

// TestTheFileIsRemovedWhenTheProcessFailsToStart pins the most likely failure.
//
// The connector binary is missing, or refuses its arguments, and start returns an
// error. The token is on disk at that moment, and nothing else will come back for it.
func TestTheFileIsRemovedWhenTheProcessFailsToStart(t *testing.T) {
	dir := t.TempDir()
	startErr := errors.New("cloudflared: no such file or directory")

	var seen string
	_, err := withCredentialFileUntilReady(dir, []byte("a-real-looking-token"),
		func(path string) (string, error) {
			// The file exists while start runs: that is the whole point of it.
			if _, statErr := os.Stat(path); statErr != nil {
				t.Errorf("the credential file does not exist when start is called: %v", statErr)
			}
			seen = path
			return "", startErr
		}, nil)

	if !errors.Is(err, startErr) {
		t.Fatalf("err = %v, want the start failure", err)
	}
	assertNoCredentialFiles(t, dir, seen)
}

// TestTheFileIsRemovedWhenReadinessFails pins the subtler failure.
//
// The process started, so something is running and may still be holding the token —
// but readiness never arrived. The file is removed anyway: leaving it because the
// process might still want it is how a secret stays on disk indefinitely.
func TestTheFileIsRemovedWhenReadinessFails(t *testing.T) {
	dir := t.TempDir()
	readyErr := errors.New("connector did not report ready within the timeout")

	var seen string
	handle, err := withCredentialFileUntilReady(dir, []byte("a-real-looking-token"),
		func(path string) (string, error) {
			seen = path
			return "started", nil
		},
		func(handle string) error {
			// The file must still exist here: readiness is what it is waiting for.
			if _, statErr := os.Stat(seen); statErr != nil {
				t.Errorf("the credential file was removed before readiness: %v", statErr)
			}
			return readyErr
		})

	if !errors.Is(err, readyErr) {
		t.Fatalf("err = %v, want the readiness failure", err)
	}
	// The handle is returned despite the failure, so the caller can stop what it
	// started. Discarding it would leak the process instead of the file.
	if handle != "started" {
		t.Errorf("handle = %q; a readiness failure must still return what was started", handle)
	}
	assertNoCredentialFiles(t, dir, seen)
}

// TestTheFileIsRemovedWhenStartPanics pins the path only defer covers.
//
// A panic in start unwinds without returning, so cleanup written after the call would
// not run. This is why removal is deferred rather than placed on each exit.
func TestTheFileIsRemovedWhenStartPanics(t *testing.T) {
	dir := t.TempDir()
	var seen string

	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Error("the panic did not propagate")
			}
		}()
		_, _ = withCredentialFileUntilReady(dir, []byte("a-real-looking-token"),
			func(path string) (string, error) {
				seen = path
				panic("the connector library panicked")
			}, nil)
	}()

	assertNoCredentialFiles(t, dir, seen)
}

// TestNoFileIsLeftWhenTheDirectoryCannotBeCreated pins that a failure before the file
// exists leaves nothing behind and says why.
func TestNoFileIsLeftWhenTheDirectoryCannotBeCreated(t *testing.T) {
	// A path whose parent is a file cannot be made into a directory.
	parent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(parent, "credentials")

	started := false
	_, err := withCredentialFileUntilReady(dir, []byte("a-real-looking-token"),
		func(string) (string, error) {
			started = true
			return "", nil
		}, nil)

	if err == nil {
		t.Fatal("an uncreatable credential directory was accepted")
	}
	if started {
		t.Fatal("the process was started without a credential file")
	}
	if !strings.Contains(err.Error(), "credential dir") {
		t.Errorf("the error does not say what could not be created: %v", err)
	}
}

// TestAnEmptyDirectoryIsRefused pins that a missing configuration is refused rather
// than writing a token to a relative path.
//
// An empty directory would make filepath.Join produce a bare filename, putting the
// token in whatever the working directory happens to be.
func TestAnEmptyDirectoryIsRefused(t *testing.T) {
	if _, _, err := createCredentialFile("", []byte("a-real-looking-token")); err == nil {
		t.Fatal("an empty credential directory was accepted")
	}
}

// TestTheDirectoryIsPrivate pins that the containing directory is not readable by
// other users.
//
// The file is 0600, which is already covered. If the directory were traversable by
// others the filename is the only thing standing between them and the token, and the
// filename is not a secret.
func TestTheDirectoryIsPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "credentials")
	if err := prepareCredentialDir(dir); err != nil {
		t.Fatalf("prepareCredentialDir: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		t.Fatalf("the credential directory is %04o; other users can reach it", mode)
	}
}

// TestTheFilenameIsNotPredictable pins that two files never collide.
//
// The name is random because O_EXCL refuses an existing file: a predictable name
// would make a second concurrent start fail, and would let anything that can write to
// the directory pre-create the path.
func TestTheFilenameIsNotPredictable(t *testing.T) {
	dir := t.TempDir()
	seen := map[string]bool{}
	for i := 0; i < 25; i++ {
		path, cleanup, err := createCredentialFile(dir, []byte("a-real-looking-token"))
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		name := filepath.Base(path)
		if seen[name] {
			t.Fatalf("the filename %q was generated twice", name)
		}
		seen[name] = true
		// Every generated name has to be recognised by the sweeper, or a crash
		// leaves a file nothing will ever clean up.
		if !isCredentialFilename(name) {
			t.Fatalf("the sweeper would not recognise %q as a credential file", name)
		}
		cleanup()
	}
}

// TestCleanupIsSafeToRepeat pins that a double cleanup is not an error.
//
// withCredentialFileUntilReady defers cleanup, and a caller holding the returned
// function may also call it. Removing an absent file must be silent.
func TestCleanupIsSafeToRepeat(t *testing.T) {
	dir := t.TempDir()
	path, cleanup, err := createCredentialFile(dir, []byte("a-real-looking-token"))
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	cleanup() // must not panic
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the file survived two cleanups: %v", err)
	}
}

// TestTheSweeperSurvivesAnUnreadableEntry pins that best-effort cleanup is
// best-effort.
//
// A file it cannot remove must not stop it removing the others. A sweep that aborts
// on the first problem leaves every later token on disk.
func TestTheSweeperSurvivesAnUnreadableEntry(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	old := now.Add(-staleCredentialAge - time.Minute)

	var stale []string
	for _, name := range []string{
		"cred-00000000000000000000000000000001.tmp",
		"cred-00000000000000000000000000000002.tmp",
		"cred-00000000000000000000000000000003.tmp",
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("token"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		stale = append(stale, path)
	}

	// A subdirectory with a credential-shaped name: not a regular file, so it must
	// be skipped rather than treated as a token.
	shaped := filepath.Join(dir, "cred-0000000000000000000000000000000f.tmp")
	if err := os.Mkdir(shaped, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := sweepStaleCredentialFiles(dir, now); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	for _, path := range stale {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("the stale token %q was not removed: %v", filepath.Base(path), err)
		}
	}
	if _, err := os.Stat(shaped); err != nil {
		t.Errorf("a directory with a credential-shaped name was removed: %v", err)
	}
}

// TestTheSweeperIgnoresADirectoryItCannotRead pins that a missing directory is not a
// failure, since sweeping runs at startup before anything has been created.
func TestTheSweeperIgnoresADirectoryItCannotRead(t *testing.T) {
	if err := sweepStaleCredentialFiles(filepath.Join(t.TempDir(), "absent"), time.Now()); err != nil {
		t.Fatalf("sweeping an absent directory failed: %v", err)
	}
	if err := sweepStaleCredentialFiles("", time.Now()); err != nil {
		t.Fatalf("sweeping an unset directory failed: %v", err)
	}
}

// TestTheTokenIsWhatWasWritten pins that the file the connector reads holds the token
// and nothing else.
func TestTheTokenIsWhatWasWritten(t *testing.T) {
	dir := t.TempDir()
	token := []byte("eyJhIjoiYWNjb3VudCIsInQiOiJ0dW5uZWwifQ==")

	path, cleanup, err := createCredentialFile(dir, token)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != string(token) {
		t.Fatalf("the file holds %q, want the token exactly", contents)
	}
}

// assertNoCredentialFiles fails when anything token-shaped is left in the directory.
//
// It checks the whole directory rather than the one path, because a leak that renamed
// or duplicated the file would pass a check on a single name.
func assertNoCredentialFiles(t *testing.T, dir, expected string) {
	t.Helper()
	if expected == "" {
		t.Fatal("start was never called, so nothing was tested")
	}
	if _, err := os.Stat(expected); !os.IsNotExist(err) {
		t.Errorf("the credential file remains: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the credential directory: %v", err)
	}
	for _, entry := range entries {
		if isCredentialFilename(entry.Name()) {
			t.Errorf("a credential file was left behind: %s", entry.Name())
		}
	}
}
