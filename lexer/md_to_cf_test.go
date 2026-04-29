package lexer

import (
	"strings"
	"testing"
)

// stubMdResolver satisfies both MdPageResolver and MdAttachmentResolver with
// pre-seeded maps. It's the md_to_cf direction counterpart of stubResolver.
type stubMdResolver struct {
	links  map[string]stubLink // dest → (title, space)
	images map[string]string   // src → filename
}

type stubLink struct{ title, space string }

func (s *stubMdResolver) ResolveLink(target string) (string, string, bool) {
	l, ok := s.links[target]
	return l.title, l.space, ok
}

func (s *stubMdResolver) ResolveImage(src string) (string, bool) {
	f, ok := s.images[src]
	return f, ok
}

func runMdToCf(t *testing.T, md string, opts MdToCfOpts) string {
	t.Helper()
	out, err := MdToCf(md, opts)
	if err != nil {
		t.Fatalf("MdToCf error: %v\ninput: %q", err, md)
	}
	return out
}

func TestMdToCf_Headings(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"# Title\n", "<h1>Title</h1>"},
		{"## Two\n", "<h2>Two</h2>"},
		{"###### Six\n", "<h6>Six</h6>"},
	}
	for _, c := range cases {
		if got := runMdToCf(t, c.in, MdToCfOpts{}); got != c.want {
			t.Errorf("input %q: got %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMdToCf_Paragraph(t *testing.T) {
	in := "Hello world.\n"
	want := "<p>Hello world.</p>"
	if got := runMdToCf(t, in, MdToCfOpts{}); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMdToCf_Emphasis(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"**bold**\n", "<p><strong>bold</strong></p>"},
		{"*italic*\n", "<p><em>italic</em></p>"},
		{"~~struck~~\n", "<p><s>struck</s></p>"},
		{"***both***\n", "<p><em><strong>both</strong></em></p>"},
	}
	for _, c := range cases {
		if got := runMdToCf(t, c.in, MdToCfOpts{}); got != c.want {
			t.Errorf("input %q: got %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMdToCf_InlineCode(t *testing.T) {
	in := "Call `fmt.Println(x)` here.\n"
	want := "<p>Call <code>fmt.Println(x)</code> here.</p>"
	if got := runMdToCf(t, in, MdToCfOpts{}); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMdToCf_FencedCode_WithLang(t *testing.T) {
	in := "```go\nfmt.Println(\"hi\")\n```\n"
	want := `<ac:structured-macro ac:name="code"><ac:parameter ac:name="language">go</ac:parameter><ac:plain-text-body><![CDATA[fmt.Println("hi")
]]></ac:plain-text-body></ac:structured-macro>`
	if got := runMdToCf(t, in, MdToCfOpts{}); got != want {
		t.Errorf("got:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestMdToCf_FencedCode_NoLang(t *testing.T) {
	in := "```\nplain code\n```\n"
	got := runMdToCf(t, in, MdToCfOpts{})
	if !strings.Contains(got, `<ac:structured-macro ac:name="code">`) {
		t.Errorf("missing code macro: %s", got)
	}
	if strings.Contains(got, `ac:name="language"`) {
		t.Errorf("no-language code should not emit language parameter: %s", got)
	}
	if !strings.Contains(got, "<![CDATA[plain code") {
		t.Errorf("CDATA missing or wrong: %s", got)
	}
}

func TestMdToCf_FencedCode_LangLowercased(t *testing.T) {
	in := "```Go\nx\n```\n"
	got := runMdToCf(t, in, MdToCfOpts{})
	if !strings.Contains(got, `<ac:parameter ac:name="language">go</ac:parameter>`) {
		t.Errorf("language should be lowercased: %s", got)
	}
}

func TestMdToCf_FencedCode_CDATAEscape(t *testing.T) {
	// Code containing "]]>" must be split across CDATA sections — a single
	// CDATA cannot contain its own closing delimiter.
	in := "```\na ]]> b\n```\n"
	got := runMdToCf(t, in, MdToCfOpts{})
	if strings.Count(got, "<![CDATA[") != 2 {
		t.Errorf("expected two CDATA sections for ]]>-containing body, got %q", got)
	}
	// The raw "]]>" must not appear as a literal delimiter close — every one
	// must be followed by a fresh CDATA open.
	if strings.Contains(got, "a ]]> b") {
		t.Errorf("]]> leaked into a single CDATA section: %q", got)
	}
}

func TestMdToCf_Link_External(t *testing.T) {
	in := "See [example](https://example.com).\n"
	want := `<p>See <a href="https://example.com">example</a>.</p>`
	if got := runMdToCf(t, in, MdToCfOpts{}); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMdToCf_Link_PageResolved(t *testing.T) {
	r := &stubMdResolver{
		links: map[string]stubLink{
			"../architecture.md": {title: "Architecture", space: "DOCS"},
		},
	}
	in := "Read [the arch doc](../architecture.md).\n"
	got := runMdToCf(t, in, MdToCfOpts{Pages: r})
	want := `<p>Read <ac:link><ri:page ri:content-title="Architecture" ri:space-key="DOCS"/><ac:plain-text-link-body><![CDATA[the arch doc]]></ac:plain-text-link-body></ac:link>.</p>`
	if got != want {
		t.Errorf("got:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestMdToCf_Link_PageResolved_NoSpace(t *testing.T) {
	r := &stubMdResolver{
		links: map[string]stubLink{"a.md": {title: "Alpha"}},
	}
	in := "[a](a.md)\n"
	got := runMdToCf(t, in, MdToCfOpts{Pages: r})
	if strings.Contains(got, "ri:space-key") {
		t.Errorf("space-key attribute emitted when space is empty: %s", got)
	}
	if !strings.Contains(got, `ri:content-title="Alpha"`) {
		t.Errorf("content-title missing or wrong: %s", got)
	}
}

func TestMdToCf_Autolink(t *testing.T) {
	in := "Go to <https://example.com>.\n"
	got := runMdToCf(t, in, MdToCfOpts{})
	want := `<p>Go to <a href="https://example.com">https://example.com</a>.</p>`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMdToCf_Image_Attachment(t *testing.T) {
	r := &stubMdResolver{
		images: map[string]string{
			"../_attachments/architecture/diagram.png": "diagram.png",
		},
	}
	in := "![overview](../_attachments/architecture/diagram.png)\n"
	got := runMdToCf(t, in, MdToCfOpts{Attachments: r})
	want := `<p><ac:image ac:alt="overview"><ri:attachment ri:filename="diagram.png"/></ac:image></p>`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMdToCf_Image_External(t *testing.T) {
	in := "![logo](https://example.com/logo.png)\n"
	got := runMdToCf(t, in, MdToCfOpts{})
	want := `<p><ac:image ac:alt="logo"><ri:url ri:value="https://example.com/logo.png"/></ac:image></p>`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMdToCf_Image_NoAlt(t *testing.T) {
	in := "![](https://example.com/x.png)\n"
	got := runMdToCf(t, in, MdToCfOpts{})
	if strings.Contains(got, "ac:alt=") {
		t.Errorf("empty alt should not emit ac:alt attribute: %s", got)
	}
}

func TestMdToCf_BulletList(t *testing.T) {
	in := "- one\n- two\n"
	got := runMdToCf(t, in, MdToCfOpts{})
	want := "<ul><li>one</li><li>two</li></ul>"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMdToCf_OrderedList(t *testing.T) {
	in := "1. first\n1. second\n"
	got := runMdToCf(t, in, MdToCfOpts{})
	want := "<ol><li>first</li><li>second</li></ol>"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMdToCf_NestedList(t *testing.T) {
	in := "- outer\n  - inner\n- second\n"
	got := runMdToCf(t, in, MdToCfOpts{})
	want := "<ul><li>outer<ul><li>inner</li></ul></li><li>second</li></ul>"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMdToCf_TaskList(t *testing.T) {
	in := "- [ ] do thing\n- [x] done thing\n"
	got := runMdToCf(t, in, MdToCfOpts{})
	want := `<ac:task-list><ac:task><ac:task-status>incomplete</ac:task-status><ac:task-body>do thing</ac:task-body></ac:task><ac:task><ac:task-status>complete</ac:task-status><ac:task-body>done thing</ac:task-body></ac:task></ac:task-list>`
	if got != want {
		t.Errorf("got:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestMdToCf_MixedList_NotTaskList(t *testing.T) {
	// A list where some items have checkboxes and some don't should render
	// as a regular bullet list (with literal "[x]"/"[ ]" for the boxed ones)
	// rather than as a Confluence task macro. This avoids silently dropping
	// the non-task items.
	in := "- [x] checked\n- plain\n"
	got := runMdToCf(t, in, MdToCfOpts{})
	if strings.Contains(got, "<ac:task-list>") {
		t.Errorf("mixed list should not render as task-list: %s", got)
	}
	if !strings.Contains(got, "<ul>") {
		t.Errorf("expected ul for mixed list: %s", got)
	}
}

func TestMdToCf_Blockquote(t *testing.T) {
	in := "> quoted text\n"
	got := runMdToCf(t, in, MdToCfOpts{})
	want := "<blockquote><p>quoted text</p></blockquote>"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMdToCf_ThematicBreak(t *testing.T) {
	in := "a\n\n---\n\nb\n"
	got := runMdToCf(t, in, MdToCfOpts{})
	want := "<p>a</p><hr/><p>b</p>"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMdToCf_HardBreak(t *testing.T) {
	in := "line one\\\nline two\n"
	got := runMdToCf(t, in, MdToCfOpts{})
	if !strings.Contains(got, "<br/>") {
		t.Errorf("expected <br/> for hard break: %q", got)
	}
}

func TestMdToCf_Table(t *testing.T) {
	in := "| Name | Type |\n| --- | --- |\n| id | int |\n| name | string |\n"
	got := runMdToCf(t, in, MdToCfOpts{})
	want := "<table><tbody><tr><th>Name</th><th>Type</th></tr><tr><td>id</td><td>int</td></tr><tr><td>name</td><td>string</td></tr></tbody></table>"
	if got != want {
		t.Errorf("got:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestMdToCf_Table_AlignmentEmittedAsCellStyle(t *testing.T) {
	// Per-column alignment from the GFM separator row must travel on every
	// header AND data cell as `style="text-align: …"`, since Confluence's
	// storage doesn't have a colgroup equivalent. Pre-fix the writer
	// dropped alignment entirely, so any push of an aligned table came
	// back from a pull as a plain |---|---| table.
	in := "| L | C | R |\n|:---|:---:|---:|\n| a | b | c |\n"
	got := runMdToCf(t, in, MdToCfOpts{})
	for _, want := range []string{
		`<th style="text-align: left;">L</th>`,
		`<th style="text-align: center;">C</th>`,
		`<th style="text-align: right;">R</th>`,
		`<td style="text-align: left;">a</td>`,
		`<td style="text-align: center;">b</td>`,
		`<td style="text-align: right;">c</td>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in storage XML, got:\n%s", want, got)
		}
	}

	// Unaligned columns must not get a stray style attribute.
	in2 := "| A | B |\n| --- | --- |\n| 1 | 2 |\n"
	got2 := runMdToCf(t, in2, MdToCfOpts{})
	if strings.Contains(got2, "style=") {
		t.Errorf("unaligned table should not emit style attributes, got:\n%s", got2)
	}
}

func TestRoundTrip_Table_PreservesAlignment(t *testing.T) {
	// A user's table with center alignment must survive md → storage →
	// md unchanged. This is the regression check for the user's reported
	// "all columns came back as |---|---|" symptom.
	in := "| Name | Score | Owner |\n| --- |:---:| --- |\n| a | 9 | x |\n| b | 5 | y |\n"
	xml := runMdToCf(t, in, MdToCfOpts{})
	back := runCfToMd(t, xml, CfToMdOpts{})
	if !strings.Contains(back, ":---:") {
		t.Errorf("center alignment lost in round trip:\n%s", back)
	}
}

func TestMdToCf_Fence_SplicedVerbatim(t *testing.T) {
	// The fence round trip — an HTML block matching our v1/b64 shape is
	// decoded and the original storage XML is spliced in unchanged. This is
	// the whole point of fence preservation.
	originalXML := `<ac:structured-macro ac:name="jira"><ac:parameter ac:name="key">PROJ-1</ac:parameter></ac:structured-macro>`
	fenced := EncodeBlockFence(originalXML)
	md := "Before.\n\n" + fenced + "\n\nAfter.\n"

	got := runMdToCf(t, md, MdToCfOpts{})
	if !strings.Contains(got, originalXML) {
		t.Errorf("fenced XML not spliced back:\n%s", got)
	}
	if strings.Contains(got, "confluencer:storage") {
		t.Errorf("fence marker leaked into storage output: %s", got)
	}
}

func TestMdToCf_PlainHTMLBlock_EscapedParagraph(t *testing.T) {
	// Non-fence HTML gets wrapped in <p> and its contents escaped. This
	// matches CLAUDE.md's guidance — don't emit ac:structured-macro
	// ac:name="html" because many Cloud instances disable it.
	in := "<div class=\"oops\">raw html</div>\n"
	got := runMdToCf(t, in, MdToCfOpts{})
	if !strings.HasPrefix(got, "<p>") {
		t.Errorf("expected paragraph wrapping for raw HTML: %s", got)
	}
	if !strings.Contains(got, "&lt;div") {
		t.Errorf("raw HTML should be escaped: %s", got)
	}
}

func TestMdToCf_RawInlineHTML_Escaped(t *testing.T) {
	in := "this has <span>inline</span> html.\n"
	got := runMdToCf(t, in, MdToCfOpts{})
	if !strings.Contains(got, "&lt;span&gt;") {
		t.Errorf("raw inline HTML should be escaped: %s", got)
	}
}

func TestMdToCf_SpecialChars_Escaped(t *testing.T) {
	in := "a & b < c > d\n"
	got := runMdToCf(t, in, MdToCfOpts{})
	if !strings.Contains(got, "&amp;") || !strings.Contains(got, "&lt;") || !strings.Contains(got, "&gt;") {
		t.Errorf("special chars not escaped: %s", got)
	}
}

func TestMdToCf_Empty(t *testing.T) {
	got := runMdToCf(t, "", MdToCfOpts{})
	if got != "" {
		t.Errorf("expected empty output, got %q", got)
	}
}

func TestMdToCf_NilResolvers_FallsBackToExternal(t *testing.T) {
	// With no resolvers, .md link targets render as plain <a href>. This is
	// deliberately lossy but never silent — a subsequent pull will see the
	// external-shaped link and not treat it as a page reference.
	in := "[doc](other.md)\n"
	got := runMdToCf(t, in, MdToCfOpts{})
	if !strings.Contains(got, `<a href="other.md">`) {
		t.Errorf("expected external link fallback: %s", got)
	}
	if strings.Contains(got, "ac:link") {
		t.Errorf("page link emitted without resolver: %s", got)
	}
}
