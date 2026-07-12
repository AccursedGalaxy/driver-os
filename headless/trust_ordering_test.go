package headless

import (
	"flag"
	"io"
	"testing"
)

type countingReader struct{ reads int }

func (r *countingReader) Read(p []byte) (int, error) {
	r.reads++
	return 0, io.EOF
}

func TestTrustRefusalDoesNotLoadEnvironmentOrConsumeTaskStdin(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "missing trust", args: []string{"-ledger=false", "-task=-"}},
		{name: "invalid trust", args: []string{"-ledger=false", "-trust=bogus", "-task=-"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldLoadDotenv, oldTaskStdin, oldErrW := loadDotenv, taskStdin, errW
			t.Cleanup(func() {
				loadDotenv, taskStdin, errW = oldLoadDotenv, oldTaskStdin, oldErrW
			})

			loads := 0
			loadDotenv = func() { loads++ }
			stdin := &countingReader{}
			taskStdin = stdin
			errW = io.Discard

			if got := runMain(newTestFlagSet(), tc.args); got != 1 {
				t.Fatalf("runMain(%v) exit code = %d, want setup-error 1", tc.args, got)
			}
			if loads != 0 {
				t.Fatalf("loadDotenv called %d times on trust refusal, want 0", loads)
			}
			if stdin.reads != 0 {
				t.Fatalf("-task=- stdin was read %d times on trust refusal, want 0", stdin.reads)
			}
		})
	}
}

func TestTrustConsentPrecedesDotenvLoad(t *testing.T) {
	oldLoadDotenv, oldErrW := loadDotenv, errW
	t.Cleanup(func() { loadDotenv, errW = oldLoadDotenv, oldErrW })

	loads := 0
	loadDotenv = func() { loads++ }
	errW = io.Discard

	args := []string{"-ledger=false", "-trust=trusted-local", "-effort=bogus"}
	if got := runMain(newTestFlagSet(), args); got != 1 {
		t.Fatalf("runMain(%v) exit code = %d, want setup-error 1", args, got)
	}
	if loads != 1 {
		t.Fatalf("loadDotenv called %d times after valid trust, want 1", loads)
	}
}

func newTestFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}
