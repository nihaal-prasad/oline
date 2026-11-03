package llm

import "testing"

// run feeds the chunks through a splitter and returns the concatenated text and
// reasoning streams (then flushes).
func run(chunks ...string) (text, think string) {
	sp := &thinkSplitter{}
	et := func(s string) error { text += s; return nil }
	eh := func(s string) error { think += s; return nil }
	for _, c := range chunks {
		_ = sp.feed(c, et, eh)
	}
	_ = sp.flush(et, eh)
	return
}

func TestThinkSplitter(t *testing.T) {
	cases := []struct {
		name      string
		chunks    []string
		wantText  string
		wantThink string
	}{
		{"plain", []string{"hello world"}, "hello world", ""},
		{"wrapped", []string{"<think>planning</think>answer"}, "answer", "planning"},
		{"leading text", []string{"pre <think>mid</think> post"}, "pre  post", "mid"},
		{"thinking variant", []string{"<thinking>deep</thinking>out"}, "out", "deep"},
		{"multiple spans", []string{"a<think>x</think>b<think>y</think>c"}, "abc", "xy"},
		// a tag split across chunk boundaries must still be classified correctly.
		{"split open", []string{"ans<thi", "nk>secret</think>more"}, "ansmore", "secret"},
		{"split close", []string{"<think>hid", "den</thi", "nk>shown"}, "shown", "hidden"},
		// an unterminated think span is still reasoning, flushed at the end.
		{"unterminated", []string{"<think>still thinking"}, "", "still thinking"},
		// a dangling '<' that never becomes a tag is plain text after flush.
		{"false start", []string{"a < b"}, "a < b", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text, think := run(c.chunks...)
			if text != c.wantText {
				t.Errorf("text = %q, want %q", text, c.wantText)
			}
			if think != c.wantThink {
				t.Errorf("think = %q, want %q", think, c.wantThink)
			}
		})
	}
}
