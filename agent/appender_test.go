package agent

import (
	"bytes"
	"context"
	"io/fs"
	"strings"
	"testing"

	"github.com/AccursedGalaxy/driver-os/sandbox"
)

// Review #8: write_file append prefers the sandbox's Appender capability (no
// whole-file slurp); a backend without it degrades to read+concat+write only
// while the file stays under the read bound — past it the tool rejects with a
// recovery instruction instead of loading (or worse, silently truncating) it.

// plainSandbox is a minimal in-memory sandbox.Sandbox with NO optional
// capabilities — the shape of an exotic third-party backend.
type plainSandbox struct {
	files  map[string][]byte
	writes int
}

func (p *plainSandbox) Exec(context.Context, sandbox.Command) (*sandbox.Result, error) {
	return &sandbox.Result{}, nil
}
func (p *plainSandbox) Capabilities() sandbox.Capabilities { return sandbox.Capabilities{} }
func (p *plainSandbox) ReadFile(_ context.Context, path string) ([]byte, error) {
	if b, ok := p.files[path]; ok {
		return b, nil
	}
	return nil, fs.ErrNotExist
}
func (p *plainSandbox) WriteFile(_ context.Context, path string, data []byte, _ fs.FileMode) error {
	p.writes++
	p.files[path] = data
	return nil
}
func (p *plainSandbox) Remove(_ context.Context, path string) error {
	delete(p.files, path)
	return nil
}
func (p *plainSandbox) ListDir(context.Context, string) ([]sandbox.DirEntry, error) {
	return nil, nil
}
func (p *plainSandbox) Close() error { return nil }

func TestWriteFileAppendUsesAppenderCapability(t *testing.T) {
	// Through a real local sandbox (which has Appender) the pieces assemble
	// without a rewrite.
	sb := sbWith(t, map[string]string{"f.txt": "one\n"})
	if _, err := writeFileOp(context.Background(), sb, "f.txt", "two\n", true); err != nil {
		t.Fatal(err)
	}
	data, err := sb.ReadFile(context.Background(), "f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "one\ntwo\n" {
		t.Errorf("file = %q, want the piece appended", data)
	}
}

func TestWriteFileAppendFallbackStillConcatenates(t *testing.T) {
	p := &plainSandbox{files: map[string][]byte{"f.txt": []byte("one\n")}}
	out, err := writeFileOp(context.Background(), p, "f.txt", "two\n", true)
	if err != nil {
		t.Fatal(err)
	}
	if string(p.files["f.txt"]) != "one\ntwo\n" {
		t.Errorf("file = %q, want concatenated", p.files["f.txt"])
	}
	if !strings.Contains(out, "appended") {
		t.Errorf("confirmation = %q, want an 'appended' message", out)
	}
}

func TestWriteFileAppendFallbackRejectsOversizedFile(t *testing.T) {
	big := bytes.Repeat([]byte("x"), int(maxFileBytes)+1)
	p := &plainSandbox{files: map[string][]byte{"big.txt": big}}
	_, err := writeFileOp(context.Background(), p, "big.txt", "tail", true)
	if err == nil {
		t.Fatal("appending to an over-bound file on a capability-less backend must reject, not slurp")
	}
	if !strings.Contains(err.Error(), "run") {
		t.Errorf("rejection must teach the `run` redirection recovery: %v", err)
	}
	if p.writes != 0 {
		t.Errorf("the file was rewritten (%d write(s)) — a capped read would have TRUNCATED it", p.writes)
	}
}
