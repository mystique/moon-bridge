package anthropic

import "testing"

func TestThinkTagStripperWholeTag(t *testing.T) {
	var s thinkTagStripper
	out := s.Feed("之前工具调用出了问题，重新查一下。\n</think>")
	if want := "之前工具调用出了问题，重新查一下。\n"; out != want {
		t.Fatalf("Feed = %q, want %q", out, want)
	}
	if tail := s.Flush(); tail != "" {
		t.Fatalf("Flush = %q, want empty", tail)
	}
}

func TestThinkTagStripperSplitAcrossDeltas(t *testing.T) {
	var s thinkTagStripper
	got := s.Feed("答案如下：</th")
	got += s.Feed("ink>继续内容")
	got += s.Flush()
	if want := "答案如下：继续内容"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestThinkTagStripperOpenAndClose(t *testing.T) {
	var s thinkTagStripper
	got := s.Feed("<think>reasoning</think>")
	got += s.Flush()
	// Only the markers are removed; inner text preserved (see doc comment).
	if want := "reasoning"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestThinkTagStripperHoldsBackPartialPrefix(t *testing.T) {
	// "<" alone is a valid tag prefix → must be held back, not emitted yet.
	var s thinkTagStripper
	out := s.Feed("hello <")
	if want := "hello "; out != want {
		t.Fatalf("Feed = %q, want %q", out, want)
	}
	// Next delta resolves "<" as not-a-tag (followed by 'x'); flush held bytes.
	got := out + s.Feed("x world") + s.Flush()
	if want := "hello <x world"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestThinkTagStripperNoTagsPassThrough(t *testing.T) {
	var s thinkTagStripper
	in := "普通文本，没有任何标签。plain ascii no tags here"
	got := s.Feed(in) + s.Flush()
	if got != in {
		t.Fatalf("got %q, want %q", got, in)
	}
}

func TestThinkTagStripperTrailingPartialFlushed(t *testing.T) {
	// A dangling "</thi" at end of stream with no resolution is emitted as-is.
	var s thinkTagStripper
	got := s.Feed("done</thi") + s.Flush()
	if want := "done</thi"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestThinkTagStripperMultipleTagsOneDelta(t *testing.T) {
	var s thinkTagStripper
	got := s.Feed("</think>a<think>b</think>c") + s.Flush()
	if want := "abc"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
