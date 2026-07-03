package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/sandbox/local"
)

func TestFenceLargeFiles(t *testing.T) {
	root := t.TempDir()
	sb, err := local.New(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sb.Close() })

	// maxFileBytes is 1 MiB.
	const MiB = 1024 * 1024

	// (a) >1MiB fenced file, append past the boundary -> drift detected
	largePath := "large_test.go"
	largeContent := strings.Repeat("a", MiB+100)
	if err := os.WriteFile(filepath.Join(root, largePath), []byte(largeContent), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	f := newFenceState(ctx, Config{Sandbox: sb, TestFence: []string{"*_test.go"}})
	if f.snapErr != nil {
		t.Fatalf("snapshot failed: %v", f.snapErr)
	}

	// Verify it's over-cap and has no content stored.
	if ff, ok := f.base[largePath]; !ok || ff.content != nil || ff.fullHash == "" {
		t.Fatalf("large file snapshot state wrong: ok=%v, contentLen=%d, fullHash=%q", ok, len(ff.content), ff.fullHash)
	}

	// Append past the boundary.
	if err := os.WriteFile(filepath.Join(root, largePath), []byte(largeContent+"b"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := f.drift(ctx, sb); len(got) != 1 || got[0] != largePath {
		t.Fatalf("(a) drift = %v, want [%s]", got, largePath)
	}

	// (b) 3*maxFileBytes fenced file, same-size single-byte WriteAt at offset maxFileBytes+10 -> drift detected
	threeMiBPath := "three_test.go"
	threeMiBContent := []byte(strings.Repeat("c", 3*MiB))
	if err := os.WriteFile(filepath.Join(root, threeMiBPath), threeMiBContent, 0o644); err != nil {
		t.Fatal(err)
	}
	f = newFenceState(ctx, Config{Sandbox: sb, TestFence: []string{"*_test.go"}})

	// Mutate one byte at offset MiB+10.
	threeMiBContent[MiB+10] = 'd'
	if err := os.WriteFile(filepath.Join(root, threeMiBPath), threeMiBContent, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := f.drift(ctx, sb); len(got) != 1 || got[0] != threeMiBPath {
		t.Fatalf("(b) drift = %v, want [%s]", got, threeMiBPath)
	}

	// (c) same-size tail mutation on a 1MiB+100 file -> detected
	tailPath := "tail_test.go"
	tailContent := []byte(strings.Repeat("e", MiB+100))
	if err := os.WriteFile(filepath.Join(root, tailPath), tailContent, 0o644); err != nil {
		t.Fatal(err)
	}
	f = newFenceState(ctx, Config{Sandbox: sb, TestFence: []string{"*_test.go"}})
	tailContent[len(tailContent)-1] = 'f'
	if err := os.WriteFile(filepath.Join(root, tailPath), tailContent, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := f.drift(ctx, sb); len(got) != 1 || got[0] != tailPath {
		t.Fatalf("(c) drift = %v, want [%s]", got, tailPath)
	}

	// (d) over-cap file: snapshot stores nil content; restore returns an error and does NOT truncate
	if err := f.restore(ctx, sb, []string{tailPath}); err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("(d) restore should fail for over-cap file, got %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, tailPath)); len(data) != MiB+100 || data[len(data)-1] != 'f' {
		t.Fatalf("(d) file should not have been truncated or restored: len=%d, lastByte=%c", len(data), data[len(data)-1])
	}

	// (e) leading-dash filename "-large_test.go", over-cap, append -> detected
	dashPath := "-large_test.go"
	dashContent := strings.Repeat("g", MiB+100)
	if err := os.WriteFile(filepath.Join(root, dashPath), []byte(dashContent), 0o644); err != nil {
		t.Fatal(err)
	}
	f = newFenceState(ctx, Config{Sandbox: sb, TestFence: []string{"*_test.go"}})
	if err := os.WriteFile(filepath.Join(root, dashPath), []byte(dashContent+"h"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := f.drift(ctx, sb); len(got) != 1 || got[0] != dashPath {
		t.Fatalf("(e) drift = %v, want [%s]", got, dashPath)
	}
}
