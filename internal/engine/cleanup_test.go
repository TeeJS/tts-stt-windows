package engine

import "testing"

func TestCleanTranscript(t *testing.T) {
	cases := []struct{ in, want, why string }{
		// Pure noise annotations: nothing was said, so nothing should be sent.
		{"(clicking)", "", "keyboard clicks"},
		{"[BLANK_AUDIO]", "", "silence marker"},
		{"(buzzing)", "", "fan noise"},
		{" (keyboard clacking) ", "", "padded annotation"},
		{"*sighs*", "", "asterisk annotation"},
		{"♪♪♪", "", "music notes"},
		{"♪ upbeat music ♪", "", "annotated music"},
		{"...", "", "punctuation only"},
		{"", "", "empty"},
		{"   ", "", "whitespace"},

		// Known hallucinations, whole-transcript only.
		{"Thank you.", "", "stock phrase"},
		{"Thanks for watching!", "", "stock phrase with punctuation"},
		{"Subtitles by the Amara.org community", "", "subtitle credit"},

		// Real speech must survive untouched.
		{"Run the tests again", "Run the tests again", "plain speech"},
		{"Thanks for watching the deploy, it worked", "Thanks for watching the deploy, it worked",
			"stock words inside a real sentence"},
		{"Commit this and push it.", "Commit this and push it.", "normal sentence"},

		// Mixed: strip the aside, keep the words.
		{"(clicking) open the config file", "open the config file", "leading annotation"},
		{"open the config file (clicking)", "open the config file", "trailing annotation"},
		{"restart it [BLANK_AUDIO] then check the log", "restart it then check the log", "inline annotation"},
		// Anything the model marked as music/singing is dropped whole: it heard audio that wasn't
		// the user addressing it, so passing the words along would send a song as a command.
		{"♪ let's ship it ♪", "", "music markers mean it heard singing, not speech"},
	}
	for _, c := range cases {
		if got := CleanTranscript(c.in); got != c.want {
			t.Errorf("%s: CleanTranscript(%q) = %q, want %q", c.why, c.in, got, c.want)
		}
	}
}

// Parentheses appear in real dictation too; only short bracketed asides should be treated as
// annotations, and never at the cost of the sentence around them.
func TestCleanTranscriptKeepsSpeech(t *testing.T) {
	long := "the function (which we rewrote last week after the incident that took down the panel) still fails"
	if got := CleanTranscript(long); got != long {
		t.Errorf("long parenthetical was stripped:\n got %q\nwant %q", got, long)
	}
}
