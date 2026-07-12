package headless

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// errW is the live-trace sink for every stderr write in this package (D1:
// stderr is the diagnostics channel). setupTrace swaps it for a filtering
// writer under -trace=compact / -trace-file; it defaults to the real stderr so
// pre-setup errors are never lost.
var errW io.Writer = os.Stderr

// setupTrace validates -trace and installs the trace pipeline. It returns a
// closer that flushes any partial trailing line (nil when no wrapping was
// needed). The full unfiltered stream is teed to traceFile when set,
// regardless of mode — debug a compact run from the file, not the terminal.
func setupTrace(mode, traceFile string) (io.Closer, error) {
	var compact bool
	switch mode {
	case "full":
	case "compact":
		compact = true
	default:
		return nil, fmt.Errorf("unknown -trace %q (want 'full' or 'compact')", mode)
	}
	var fh *os.File
	if traceFile != "" {
		var err error
		if fh, err = os.Create(traceFile); err != nil {
			return nil, fmt.Errorf("-trace-file: %v", err)
		}
	}
	if !compact && fh == nil {
		return nil, nil // plain full trace to stderr: no wrapping needed
	}
	tw := &traceWriter{dst: os.Stderr, file: fh, compact: compact, now: time.Now}
	errW = tw
	return tw, nil
}

// traceWriter forwards the line-oriented live trace. In compact mode it
// reduces the stream to an orchestrator heartbeat — one line per iteration
// (timestamp, iteration, cumulative tokens) plus gate milestones — replacing
// the tee+awk filter delegate.sh used to carry. The full stream still lands in
// the optional trace file. Line-buffered and mutex-guarded: observer and
// banner writes may interleave from different goroutines.
type traceWriter struct {
	mu      sync.Mutex
	dst     io.Writer // the real stderr
	file    *os.File  // optional unfiltered tee
	compact bool
	pend    []byte
	iter    string // last seen "N/M" iteration marker, for token heartbeat lines
	now     func() time.Time
}

func (t *traceWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pend = append(t.pend, p...)
	for {
		i := bytes.IndexByte(t.pend, '\n')
		if i < 0 {
			break
		}
		t.emit(string(t.pend[:i+1]))
		t.pend = t.pend[i+1:]
	}
	return len(p), nil
}

// Close flushes a partial trailing line and closes the tee file.
func (t *traceWriter) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.pend) > 0 {
		t.emit(string(t.pend) + "\n")
		t.pend = nil
	}
	if t.file != nil {
		return t.file.Close()
	}
	return nil
}

// milestonePrefixes are the line starts that survive compact mode (the same
// set delegate.sh's awk filter kept): gate and lifecycle banners plus warnings
// and errors. Model/observation content is dropped — it lives in -trace-file
// and the transcript.
var milestonePrefixes = []string{
	"review", "finish", "verify", "plan", "outcome", "budget", "worktree",
	"baseline", "fence", "scope", "ledger", "report", "transcript",
	"WARN", "warn", "error", "Error",
}

// humanTokens renders a cumulative token count at a readable scale.
func humanTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	default:
		return strconv.Itoa(n)
	}
}

func (t *traceWriter) emit(line string) {
	if t.file != nil {
		io.WriteString(t.file, line)
	}
	if !t.compact {
		io.WriteString(t.dst, line)
		return
	}
	trimmed := strings.TrimRight(line, "\n")
	switch {
	case strings.HasPrefix(trimmed, "========== iteration "):
		if fields := strings.Fields(trimmed); len(fields) >= 3 {
			t.iter = fields[2]
		}
	case strings.HasPrefix(trimmed, "tokens: "):
		if fields := strings.Fields(trimmed); len(fields) >= 2 {
			if n, err := strconv.Atoi(fields[1]); err == nil {
				fmt.Fprintf(t.dst, "[%s] iter %s · %s tok\n", t.now().Format("15:04:05"), t.iter, humanTokens(n))
				return
			}
		}
	default:
		for _, p := range milestonePrefixes {
			if strings.HasPrefix(trimmed, p) {
				fmt.Fprintf(t.dst, "[%s] %s\n", t.now().Format("15:04:05"), trimmed)
				return
			}
		}
	}
}
