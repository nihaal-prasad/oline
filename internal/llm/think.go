package llm

import "strings"

// Some providers (e.g. Fireworks GLM / DeepSeek variants) stream a model's
// chain-of-thought INLINE in the content, wrapped in <think>…</think> (or
// <thinking>…</thinking>), instead of on a separate reasoning channel. Left as
// plain content it would be indistinguishable from the answer — so the TUI
// couldn't hide it with the "thoughts" toggle, and it would pollute history.
//
// thinkSplitter consumes content deltas as they arrive and routes the spans
// inside those tags to the reasoning channel and everything else to the text
// channel. It is stateful across deltas: a tag may be split across chunks, so it
// holds back only a trailing fragment that could be the start of a tag.
type thinkSplitter struct {
	inThink bool
	buf     string // held-back tail (a possible partial tag boundary)
}

var (
	thinkOpen  = []string{"<think>", "<thinking>"}
	thinkClose = []string{"</think>", "</thinking>"}
)

func maxLen(toks []string) int {
	m := 0
	for _, t := range toks {
		if len(t) > m {
			m = len(t)
		}
	}
	return m
}

// firstTok returns the earliest index at which any of toks occurs in s, and the
// matching token; idx is -1 when none occur.
func firstTok(s string, toks []string) (idx int, tok string) {
	idx = -1
	for _, t := range toks {
		if i := strings.Index(s, t); i >= 0 && (idx == -1 || i < idx) {
			idx, tok = i, t
		}
	}
	return
}

// partialSuffix returns the length of the longest suffix of s that is a proper
// prefix of some token in toks (so it might be the start of a tag continuing in
// the next chunk). It never returns len(s) for a full token (that is matched by
// firstTok instead); here we only hold back incomplete boundaries.
func partialSuffix(s string, toks []string) int {
	max := maxLen(toks) - 1
	if max > len(s) {
		max = len(s)
	}
	for l := max; l > 0; l-- {
		suf := s[len(s)-l:]
		for _, t := range toks {
			if len(suf) < len(t) && strings.HasPrefix(t, suf) {
				return l
			}
		}
	}
	return 0
}

// feed processes one content delta, emitting text outside think-tags via emitTxt
// and reasoning inside them via emitThink. Partial tags at the end are buffered.
func (sp *thinkSplitter) feed(content string, emitTxt, emitThink func(string) error) error {
	s := sp.buf + content
	sp.buf = ""
	for len(s) > 0 {
		if !sp.inThink {
			i, tok := firstTok(s, thinkOpen)
			if i < 0 {
				// No full open tag; emit all but a possible partial-tag tail.
				hold := partialSuffix(s, thinkOpen)
				if out := s[:len(s)-hold]; out != "" {
					if err := emitTxt(out); err != nil {
						return err
					}
				}
				sp.buf = s[len(s)-hold:]
				return nil
			}
			if out := s[:i]; out != "" {
				if err := emitTxt(out); err != nil {
					return err
				}
			}
			s = s[i+len(tok):]
			sp.inThink = true
		} else {
			i, tok := firstTok(s, thinkClose)
			if i < 0 {
				hold := partialSuffix(s, thinkClose)
				if out := s[:len(s)-hold]; out != "" {
					if err := emitThink(out); err != nil {
						return err
					}
				}
				sp.buf = s[len(s)-hold:]
				return nil
			}
			if out := s[:i]; out != "" {
				if err := emitThink(out); err != nil {
					return err
				}
			}
			s = s[i+len(tok):]
			sp.inThink = false
		}
	}
	return nil
}

// flush emits any buffered tail at end of stream (an unterminated think span is
// still reasoning; a dangling partial tag is just text).
func (sp *thinkSplitter) flush(emitTxt, emitThink func(string) error) error {
	if sp.buf == "" {
		return nil
	}
	out := sp.buf
	sp.buf = ""
	if sp.inThink {
		return emitThink(out)
	}
	return emitTxt(out)
}
