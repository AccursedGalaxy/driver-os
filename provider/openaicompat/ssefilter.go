package openaicompat

// SSE keep-alive filter. OpenRouter emits comment lines (": OPENROUTER
// PROCESSING" + a blank line) while a slow upstream spins up — legal SSE, but
// the SDK's decoder (openai-go packages/ssestream) dispatches the trailing
// blank line as an event with EMPTY data and json.Unmarshal("") then kills the
// stream. Per the SSE spec an event with an empty data buffer must not be
// dispatched at all, so stripping comments AND the blank lines that would
// dispatch an empty event is semantics-preserving for any compliant payload.
// Scoped to this provider via request middleware rather than the SDK's global
// RegisterDecoder registry (which would mutate every openai-go user in the
// process and keys on an exact content-type string match anyway).

import (
	"io"
	"net/http"
	"strings"

	"github.com/openai/openai-go/option"
)

// sseKeepaliveMiddleware wraps streaming response bodies in the comment filter.
// Non-SSE responses (plain Generate calls, errors) pass through untouched.
func sseKeepaliveMiddleware(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
	resp, err := next(req)
	if err == nil && resp != nil &&
		strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		resp.Body = newSSECommentFilter(resp.Body)
	}
	return resp, err
}

// sseCommentFilter is a line-oriented streaming transform: it drops SSE comment
// lines and any blank line reached with no field line emitted since the last
// dispatch (the empty-event case). It never buffers past the bytes it has
// already received — field-line bytes are forwarded as they arrive, so the
// live-typing latency of streaming is preserved.
type sseCommentFilter struct {
	rc  io.ReadCloser
	err error  // deferred read error, returned once out is drained
	out []byte // transformed bytes ready to serve
	buf [4096]byte

	midLine   bool // inside a line whose bytes are being forwarded
	dropLine  bool // inside a comment line being discarded
	pendingCR bool // '\r' seen at line start; blank iff '\n' follows
	sawField  bool // a field line was emitted since the last dispatched blank
}

func newSSECommentFilter(rc io.ReadCloser) *sseCommentFilter {
	return &sseCommentFilter{rc: rc}
}

func (f *sseCommentFilter) Read(p []byte) (int, error) {
	for len(f.out) == 0 && f.err == nil {
		n, err := f.rc.Read(f.buf[:])
		if n > 0 {
			f.transform(f.buf[:n])
		}
		f.err = err
	}
	if len(f.out) == 0 {
		return 0, f.err
	}
	n := copy(p, f.out)
	f.out = f.out[n:]
	return n, nil
}

func (f *sseCommentFilter) Close() error { return f.rc.Close() }

// transform walks one received chunk through the line state machine, appending
// the bytes that survive to f.out.
func (f *sseCommentFilter) transform(b []byte) {
	for _, c := range b {
		switch {
		case f.dropLine: // discard the comment through its newline
			if c == '\n' {
				f.dropLine = false
			}
		case f.midLine: // field-line bytes forward verbatim
			f.out = append(f.out, c)
			if c == '\n' {
				f.midLine = false
			}
		case f.pendingCR: // line began with '\r': blank iff '\n' follows
			f.pendingCR = false
			switch c {
			case '\n':
				if f.sawField { // dispatch blank (CRLF)
					f.out = append(f.out, '\r', '\n')
					f.sawField = false
				}
			default: // rare: a field line starting with a bare CR
				f.out = append(f.out, '\r', c)
				f.sawField = true
				f.midLine = true
			}
		default: // at line start
			switch c {
			case ':':
				f.dropLine = true
			case '\n': // blank line: dispatch only if it would carry data
				if f.sawField {
					f.out = append(f.out, '\n')
					f.sawField = false
				}
			case '\r':
				f.pendingCR = true
			default:
				f.out = append(f.out, c)
				f.sawField = true
				f.midLine = true
			}
		}
	}
}
