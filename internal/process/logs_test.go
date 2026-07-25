package process

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotatingWriterAppendsWithoutTruncating(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	if err := os.WriteFile(path, []byte("existing\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	w, err := NewRotatingWriter(path, 1024, 3)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	if _, err := w.Write([]byte("appended\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Double close is idempotent.
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "existing") || !strings.Contains(string(data), "appended") {
		t.Errorf("log content wrong (truncated?): %q", data)
	}
}

func TestRotatingWriterRotatesAtThreshold(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rotate.log")

	w, err := NewRotatingWriter(path, 100, 3)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	defer w.Close()

	chunk := bytes.Repeat([]byte("a"), 60)
	if _, err := w.Write(chunk); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	// Second write would exceed 100 bytes: rotation must happen first.
	chunk2 := bytes.Repeat([]byte("b"), 60)
	if _, err := w.Write(chunk2); err != nil {
		t.Fatalf("write 2: %v", err)
	}

	rotated, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("rotated file missing: %v", err)
	}
	if !bytes.Equal(rotated, chunk) {
		t.Errorf("rotated content = %q, want 60 x 'a'", rotated)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("current file missing: %v", err)
	}
	if !bytes.Equal(current, chunk2) {
		t.Errorf("current content = %q, want 60 x 'b'", current)
	}
}

func TestRotatingWriterShiftsBackups(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shift.log")

	w, err := NewRotatingWriter(path, 10, 3)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	defer w.Close()

	// Each write is 8 bytes; every second write triggers rotation.
	for i := 0; i < 6; i++ {
		if _, err := w.Write([]byte("01234567")); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	for _, suffix := range []string{".1", ".2"} {
		if _, err := os.Stat(path + suffix); err != nil {
			t.Errorf("expected backup %s%s: %v", filepath.Base(path), suffix, err)
		}
	}
}

func TestRotatingWriterClosedWriteFails(t *testing.T) {
	dir := t.TempDir()
	w, err := NewRotatingWriter(filepath.Join(dir, "closed.log"), 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("x")); err == nil {
		t.Error("expected error writing to closed writer")
	}
}
