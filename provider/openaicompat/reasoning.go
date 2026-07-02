package openaicompat

import (
	"encoding/json"
	"fmt"
)

// reasoningAccumulator reassembles the streamed `reasoning_details` extension
// (OpenRouter) into the same array shape the non-streaming response carries, so
// a streamed turn can replay its reasoning trace exactly like a Generate one —
// without this, streaming DROPS the trace, and for Gemini's encrypted thought
// signature that means the replayed turn loses its chain of thought (the
// spiral llm.ReasoningPart exists to prevent).
//
// Wire shape (captured in testdata/openrouter-keepalive-deepseek.sse): each
// chunk's delta may carry `reasoning_details`, an array of block fragments
// keyed by `index` — plaintext blocks (`reasoning.text`) arrive as many small
// fragments whose `text` concatenates; opaque blocks (reasoning.encrypted,
// summaries) carry `data`/`summary` the same way. Every fragment repeats its
// identifying fields (type/format/index/id), so merging is: same block ⇒
// concatenate the payload strings, last write wins for everything else.
type reasoningAccumulator struct {
	entries []map[string]any
	byKey   map[string]int // block key → position in entries
}

// add folds one delta's reasoning_details array into the accumulator. Raw that
// doesn't parse as an array of objects is ignored — a malformed fragment must
// not kill a live stream over an OPTIONAL extension field.
func (a *reasoningAccumulator) add(raw json.RawMessage) {
	var incoming []map[string]any
	if json.Unmarshal(raw, &incoming) != nil {
		return
	}
	for _, e := range incoming {
		key := reasoningKey(e)
		if key == "" {
			a.entries = append(a.entries, e)
			continue
		}
		if a.byKey == nil {
			a.byKey = make(map[string]int)
		}
		if i, ok := a.byKey[key]; ok {
			mergeReasoningFragment(a.entries[i], e)
		} else {
			a.byKey[key] = len(a.entries)
			a.entries = append(a.entries, e)
		}
	}
}

// raw returns the reassembled array, or ok=false when the stream carried no
// reasoning at all (the common non-thinking case — no chunk is emitted then).
func (a *reasoningAccumulator) raw() (json.RawMessage, bool) {
	if len(a.entries) == 0 {
		return nil, false
	}
	b, err := json.Marshal(a.entries)
	if err != nil {
		return nil, false
	}
	return b, true
}

// reasoningKey identifies which block a fragment belongs to: type+index (the
// wire's block coordinates), falling back to id. Empty means "no identity" —
// such a fragment is kept as its own entry rather than merged into a wrong one.
func reasoningKey(e map[string]any) string {
	typ, _ := e["type"].(string)
	if idx, ok := e["index"].(float64); ok {
		return fmt.Sprintf("%s|%d", typ, int(idx))
	}
	if id, ok := e["id"].(string); ok && id != "" {
		return typ + "|id:" + id
	}
	return ""
}

// mergeReasoningFragment folds a later fragment of the same block into dst:
// payload strings concatenate (they stream in pieces), any other field takes
// the fragment's value (they repeat verbatim; last write is a no-op).
func mergeReasoningFragment(dst, src map[string]any) {
	for k, v := range src {
		switch k {
		case "text", "data", "summary":
			s, ok := v.(string)
			if !ok {
				dst[k] = v
				continue
			}
			if prev, ok := dst[k].(string); ok {
				dst[k] = prev + s
			} else {
				dst[k] = s
			}
		default:
			dst[k] = v
		}
	}
}
