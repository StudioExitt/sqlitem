package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// viKeys feeds a vi key string to the editor. <esc>, <enter>, <bs> and
// <space> are special keys; everything else is typed rune by rune.
func viKeys(e *editorModel, s string) string {
	var yank string
	for len(s) > 0 {
		var msg tea.KeyMsg
		switch {
		case strings.HasPrefix(s, "<esc>"):
			msg, s = tea.KeyMsg{Type: tea.KeyEsc}, s[5:]
		case strings.HasPrefix(s, "<enter>"):
			msg, s = tea.KeyMsg{Type: tea.KeyEnter}, s[7:]
		case strings.HasPrefix(s, "<bs>"):
			msg, s = tea.KeyMsg{Type: tea.KeyBackspace}, s[4:]
		case strings.HasPrefix(s, "<space>"):
			msg, s = tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}, s[7:]
		default:
			r := []rune(s)[0]
			msg, s = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}, s[len(string(r)):]
		}
		if y := e.update(msg); y != "" {
			yank = y
		}
	}
	return yank
}

// newVi returns an editor in NORMAL mode with the cursor at the '|' marker.
func newVi(text string) *editorModel {
	e := newEditorModel()
	i := strings.Index(text, "|")
	e.setText(strings.Replace(text, "|", "", 1), false)
	e.mode = modeNormal
	e.row, e.col = e.fromIndex(len([]rune(text[:i])))
	return e
}

// cursorText renders the buffer with '|' at the cursor.
func cursorText(e *editorModel) string {
	rs := e.flatRunes()
	i := e.toIndex(e.row, e.col)
	return string(rs[:i]) + "|" + string(rs[i:])
}

func TestViCommands(t *testing.T) {
	cases := []struct{ name, in, keys, want string }{
		// motions
		{"w", "|select a, b from t", "w", "select |a, b from t"},
		{"count w", "|select a, b from t", "3w", "select a, |b from t"},
		{"W", "|select a.b, c", "W", "select |a.b, c"},
		{"e", "|select abc", "e", "selec|t abc"},
		{"b", "select ab|c", "b", "select |abc"},
		{"ge", "select ab|c", "ge", "selec|t abc"},
		{"$ and 0", "ab|cd", "$", "abc|d"},
		{"^", "   ab|cd", "^", "   |abcd"},
		{"f", "|a(b(c)", "f(", "a|(b(c)"},
		{"f count", "|a(b(c)", "2f(", "a(b|(c)"},
		{"t", "|a(b(c)", "t(", "|a(b(c)"},
		{"t;", "|abcabc", "tc;", "abca|bc"},
		{"F,", "abcab|c", "Fa,", "abc|abc"},
		{"%", "|(a (b) c)", "%", "(a (b) c|)"},
		{"% back", "(a (b) c|)", "%", "|(a (b) c)"},
		{"gg G", "a\nb\n|c", "gg", "|a\nb\nc"},
		{"count G", "|a\nb\nc", "2G", "a\n|b\nc"},
		{"}", "|a\nb\n\nc", "}", "a\nb\n|\nc"},
		{"{", "a\n\nb\n|c", "{", "a\n|\nb\nc"},
		{"j keeps column", "abc|d\nx\nabcdef", "jj", "abcd\nx\nabc|def"},
		{"+ -", "|a\n  b", "+", "a\n  |b"},
		{"marks", "|a\nb\nc", "majj'a", "|a\nb\nc"},
		{"search", "|select a from t where a = 1", "/a<enter>", "select |a from t where a = 1"},
		{"search n", "|select a from t where a = 1", "/a<enter>n", "select a from t where |a = 1"},
		{"search N wraps", "|select a from t where a = 1", "/a<enter>N", "select a from t where |a = 1"},
		{"star", "|id, name, id", "*", "id, name, |id"},
		// operators
		{"dw", "|select a from t", "dw", "|a from t"},
		{"d2w", "|select a from t", "d2w", "|from t"},
		{"2dw", "|select a from t", "2dw", "|from t"},
		{"dw end of line", "select |a\nfrom t", "dw", "select| \nfrom t"},
		{"de", "|select a", "de", "| a"},
		{"cw", "|select a<x>", "cwupdate<esc>", "updat|e a<x>"},
		{"c$", "select |a from t", "c$b<esc>", "select |b"},
		{"C", "select |a from t", "Cb<esc>", "select |b"},
		{"D", "select |a from t", "D", "select| "},
		{"dd", "a\n|b\nc", "dd", "a\n|c"},
		{"2dd", "|a\nb\nc", "2dd", "|c"},
		{"dj", "|a\nb\nc", "dj", "|c"},
		{"dk", "a\nb\n|c", "dk", "|a"},
		{"dG", "a\n|b\nc", "dG", "|a"},
		{"dgg", "a\n|b\nc", "dgg", "|c"},
		{"d}", "|a\nb\n\nc", "d}", "|\nc"},
		{"dt", "|select a, b", "dt,", "|, b"},
		{"df", "|select a, b", "df,", "| b"},
		{"d%", "x |(a (b)) y", "d%", "x | y"},
		{"cc keeps indent", "  |abc\nd", "ccx<esc>", "  |x\nd"},
		{"yy p", "|a\nb", "yyp", "a\n|a\nb"},
		{"yw P", "|ab cd", "ywP", "ab| ab cd"},
		{"y$ p", "a|bc", "y$$p", "abcb|c"},
		{"3p", "|a", "yl3p", "aaa|a"},
		{">>", "|a\nb", ">>", "  |a\nb"},
		{"2>>", "|a\nb", "2>>", "  |a\n  b"},
		{"<<", "    |a", "<<", "  |a"},
		{">j", "|a\nb\nc", ">j", "  |a\n  b\nc"},
		{"gUiw", "select |name from t", "gUiw", "select |NAME from t"},
		{"guu", "|SELECT A", "guu", "|select a"},
		{"g~w", "|Abc def", "g~w", "|aBC def"},
		// text objects
		{"diw", "select na|me from t", "diw", "select | from t"},
		{"daw", "select na|me from t", "daw", "select |from t"},
		{"ciw", "select na|me from t", "ciwid<esc>", "select i|d from t"},
		{"di(", "count(a|, b)", "di(", "count(|)"},
		{"da(", "x count(a|, b) y", "da(", "x count| y"},
		{"ci( nested", "f(a, g(b|), c)", "ci(x<esc>", "f(a, g(|x), c)"},
		{"dib from open", "|(abc)", "dib", "(|)"},
		{"di\"", `say "he|llo" now`, `di"`, `say "|" now`},
		{"da'", "where a = 'x|y' and b", "da'", "where a = |and b"},
		{"ci'", "where a = 'x|y'", "ci'z<esc>", "where a = '|z'"},
		{"dip", "a\n|b\n\nc", "dip", "|\nc"},
		{"dap", "a\n|b\n\nc", "dap", "|c"},
		{"yi( p", "f(|ab) ", "yi($p", "f(ab) a|b"},
		// simple commands
		{"x", "|abc", "x", "|bc"},
		{"3x", "|abcd", "3x", "|d"},
		{"X", "ab|c", "X", "a|c"},
		{"s", "|abc", "sz<esc>", "|zbc"},
		{"S", "  |abc", "Sz<esc>", "  |z"},
		{"r", "|abc", "rx", "|xbc"},
		{"3r", "|abcd", "3rx", "xx|xd"},
		{"~", "|abc", "~~", "AB|c"},
		{"J", "|select a,\n   b from t", "J", "select a,| b from t"},
		{"3J", "|a\nb\nc", "3J", "a b| c"},
		{"gJ", "|a\n  b", "gJ", "a|  b"},
		{"o", "|a\nb", "ox<esc>", "a\n|x\nb"},
		{"O keeps indent", "  |a", "Ox<esc>", "  |x\n  a"},
		{"A", "|ab", "Ac<esc>", "ab|c"},
		{"I", "  a|b", "Ix<esc>", "  |xab"},
		{"3i", "|b", "3ia<esc>", "aa|ab"},
		{"R", "|abcd", "Rxy<esc>", "x|ycd"},
		{"u", "|abc", "xxu", "|bc"},
		{"U redo", "|abc", "xxuuU", "|bc"},
		{"dot dw", "|a b c d", "dw..", "|d"},
		{"dot ciw", "|foo bar", "ciwx<esc>w.", "x |x"},
		{"dot with count", "|abcdef", "x3.", "|ef"},
		{"dot A", "|a\nb", "A;<esc>j.", "a;\nb|;"},
		// visual
		{"v d", "|abcdef", "vlld", "|def"},
		{"v y p", "|abc", "vly$p", "abca|b"},
		{"v iw", "select na|me x", "viwd", "select | x"},
		{"V d", "a\n|b\nc", "Vjd", "|a"},
		{"V >", "|a\nb", "Vj>", "  |a\n  b"},
		{"v U", "|abc", "vlU", "|ABc"},
		{"v ~", "|abc", "vl~", "|ABc"},
		{"v r", "|abc", "vlrx", "|xxc"},
		{"v c", "|abc", "vlcz<esc>", "|zc"},
		{"v J", "|a\nb\nc", "VjjJ", "a b| c"},
		{"v o", "a|bcd", "vlohd", "|d"},
		{"v p replace", "|aa bb", "yiwwviwp", "aa a|a"},
		{"v i(", "f(a|bc)", "vi(d", "f(|)"},
	}
	for _, c := range cases {
		e := newVi(c.in)
		viKeys(e, c.keys)
		if got := cursorText(e); got != c.want {
			t.Errorf("%s: %q + %q\n got  %q\n want %q", c.name, c.in, c.keys, got, c.want)
		}
		if e.mode != modeNormal {
			t.Errorf("%s: ended in %v mode", c.name, e.mode)
		}
	}
}

func TestViYankReturnsText(t *testing.T) {
	e := newVi("|select a")
	if y := viKeys(e, "yiw"); y != "select" {
		t.Errorf("yiw yanked %q", y)
	}
	if y := viKeys(e, "Vy"); y != "select a" {
		t.Errorf("Vy yanked %q", y)
	}
}

func TestViExCommands(t *testing.T) {
	e := newVi("|select a from t\nselect a, a from u\nx")
	cases := []struct{ cmd, want string }{
		{"s/a/b/", "|select b from t\nselect a, a from u\nx"},
		{"%s/a/c/g", "select b from t\n|select c, c from u\nx"},
		{"1", "|select b from t\nselect c, c from u\nx"},
		{"%s/(select) (\\w)/\\2 \\1/", "b select from t\n|c select, c from u\nx"},
		{"$", "b select from t\nc select, c from u\n|x"},
		{"1,2s/SELECT/S/gi", "b S from t\n|c S, c from u\nx"},
		{"u", "|b select from t\nc select, c from u\nx"},
		{"2d", "b select from t\n|x"},
	}
	for _, c := range cases {
		if !e.exCommand(c.cmd) {
			t.Fatalf(":%s not handled", c.cmd)
		}
		if got := cursorText(e); got != c.want {
			t.Errorf(":%s\n got  %q\n want %q", c.cmd, got, c.want)
		}
	}
	if e.exCommand("frobnicate") {
		t.Error("unknown ex command should not be handled")
	}
	e.exCommand("s/zzz/y/")
	if !e.msgErr || !strings.Contains(e.msg, "not found") {
		t.Errorf("missing pattern message: %q", e.msg)
	}
}

func TestViPendingShowcmd(t *testing.T) {
	e := newVi("|abc")
	viKeys(e, "2d")
	if !strings.Contains(e.title(), "2d") || e.vi.idle() {
		t.Errorf("showcmd: %q", e.title())
	}
	viKeys(e, "<esc>")
	if !e.vi.idle() || e.Text() != "abc" {
		t.Errorf("esc should cancel pending command: %q", e.Text())
	}
	viKeys(e, "/ab")
	if !strings.Contains(e.title(), "/ab█") {
		t.Errorf("search prompt: %q", e.title())
	}
	viKeys(e, "<esc>")
}
