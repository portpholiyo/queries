package slackx

import "testing"

func TestEscapeBlocksMentions(t *testing.T) {
	got := Escape("hi <!channel> & <@U123> <https://evil|click>")
	want := "hi &lt;!channel&gt; &amp; &lt;@U123&gt; &lt;https://evil|click&gt;"
	if got != want {
		t.Errorf("got %q", got)
	}
}

func TestToPlain(t *testing.T) {
	cases := map[string]string{
		"hello <@U1234> there":             "hello  there",
		"see <https://a.com|the docs> now": "see the docs (https://a.com) now",
		"<https://a.com>":                  "https://a.com",
		"mail <mailto:x@y.com|x@y.com>":    "mail x@y.com",
		"join <#C123|general>":             "join #general",
		"<!here> ping":                     "ping",
		"1 &lt; 2 &amp;&amp; 3 &gt; 2":     "1 < 2 && 3 > 2",
		"unclosed <https://a.com":          "unclosed <https://a.com",
		"*bold* and `code`":                "*bold* and `code`",
		"<https://a.com|https://a.com>":    "https://a.com",
	}
	for in, want := range cases {
		if got := ToPlain(in); got != want {
			t.Errorf("ToPlain(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestTSToTime(t *testing.T) {
	tm := TSToTime("1700000000.000200")
	if tm.Unix() != 1700000000 || tm.Nanosecond() != 200000 {
		t.Errorf("got %v", tm)
	}
}
