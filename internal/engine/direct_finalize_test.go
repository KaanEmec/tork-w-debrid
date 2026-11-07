package engine

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Without a checksum there is nothing to compare, so finishing a segmented
// download must not read the file back: a write-only part file still moves
// into place.
func TestFinalizeParallelDirectWithoutChecksumSkipsReadBack(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs Unix permissions that apply to the test user")
	}
	dir := t.TempDir()
	part, dest := filepath.Join(dir, "f.part"), filepath.Join(dir, "f")
	if err := os.WriteFile(part, []byte("data"), 0o200); err != nil {
		t.Fatal(err)
	}
	if err := finalizeParallelDirect(&directItem{}, part, dest); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("dest missing: %v", err)
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Fatalf("part file still there: %v", err)
	}
}

// With a checksum the data is still verified, and a mismatch discards it.
func TestFinalizeParallelDirectWithChecksumVerifies(t *testing.T) {
	dir := t.TempDir()
	part, dest := filepath.Join(dir, "f.part"), filepath.Join(dir, "f")
	if err := os.WriteFile(part, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	it := &directItem{checksum: Checksum{Algorithm: ChecksumSHA256, Hex: "0000000000000000000000000000000000000000000000000000000000000000"}}
	if err := finalizeParallelDirect(it, part, dest); err == nil {
		t.Fatal("finalize accepted data that does not match the checksum")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("dest exists after a mismatch: %v", err)
	}
}
