package anthropic

import "strings"

// thinkTagStripper removes stray reasoning tag markers ("<think>" and
// "</think>") from a streamed text channel.
//
// Some Anthropic-compatible upstreams (e.g. certain qwen / DeepSeek-compatible
// gateways) emit the reasoning section correctly via thinking_delta but then
// leak a stray "</think>" into the text_delta stream, or wrap inline reasoning
// in "<think>...</think>" inside the text channel. Forwarding these tags to
// the client corrupts the visible output and, when echoed back in conversation
// history, degrades the model. This stripper strips the literal tag markers
// while preserving all surrounding text.
//
// It is safe across delta boundaries: a tag split across two deltas (e.g.
// "</th" then "ink>") is held back until it can be resolved.
//
// Only the tag *markers* are removed. Inner reasoning text wrapped in
// "<think>...</think>" is left intact — stripping it would risk dropping
// content when a tag is only partially received at end of stream. For the
// upstreams targeted here, reasoning already arrives via thinking_delta, so
// the only observed leak is the closing marker itself.
type thinkTagStripper struct {
	pending string // suffix of the previous delta that may be a partial tag
}

// maxTagLen is the length of the longest tag we strip ("</think>" = 8).
const maxTagLen = 8

var thinkTags = []string{"<think>", "</think>"}

// isTagPrefix reports whether s is a (possibly complete) prefix of any tag.
func isTagPrefix(s string) bool {
	for _, tag := range thinkTags {
		if strings.HasPrefix(tag, s) || strings.HasPrefix(s, tag) {
			return true
		}
	}
	return false
}

// Feed appends a delta and returns the text that is safe to emit now. Any
// trailing bytes that could still form a tag (or complete a partial one) are
// retained for the next call. Call Flush at end of stream to drain them.
func (t *thinkTagStripper) Feed(delta string) string {
	combined := t.pending + delta
	t.pending = ""

	// Repeatedly strip any complete tag occurrences.
	for {
		idx, length := earliestTag(combined)
		if idx < 0 {
			break
		}
		combined = combined[:idx] + combined[idx+length:]
	}

	// Hold back the longest trailing suffix that is still a potential partial
	// tag, so a split tag can be resolved with the next delta.
	cut := len(combined)
	for keep := 1; keep <= maxTagLen-1 && keep <= len(combined); keep++ {
		suffix := combined[len(combined)-keep:]
		if isTagPrefix(suffix) {
			cut = len(combined) - keep
		}
	}
	if cut < 0 {
		cut = 0
	}
	t.pending = combined[cut:]
	return combined[:cut]
}

// Flush returns any text still held back because it could have been a partial
// tag. After Flush the stripper is empty.
func (t *thinkTagStripper) Flush() string {
	out := t.pending
	t.pending = ""
	// At end of stream, strip any complete tags that remained (e.g. a trailing
	// "</think>" delivered whole in the final delta). Leftover partials are
	// emitted as-is rather than dropped.
	for {
		idx, length := earliestTag(out)
		if idx < 0 {
			break
		}
		out = out[:idx] + out[idx+length:]
	}
	return out
}

// earliestTag returns the index and length of the earliest complete tag
// occurrence in s, or (-1, 0) if none.
func earliestTag(s string) (int, int) {
	best := -1
	bestLen := 0
	for _, tag := range thinkTags {
		if i := strings.Index(s, tag); i >= 0 && (best == -1 || i < best) {
			best = i
			bestLen = len(tag)
		}
	}
	return best, bestLen
}

// stripThinkTags runs delta through the per-index stripper, creating it lazily
// on first use for the index. Returns the text safe to emit now; an empty
// string means the delta was entirely held back (a possible partial tag).
func (s *streamConverterState) stripThinkTags(index int, delta string) string {
	stripper := s.textStrippers[index]
	if stripper == nil {
		stripper = &thinkTagStripper{}
		s.textStrippers[index] = stripper
	}
	return stripper.Feed(delta)
}
