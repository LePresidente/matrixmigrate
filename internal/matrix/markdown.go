package matrix

import (
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
)

// The Matrix spec permits only a fixed subset of HTML in formatted_body, so several Mattermost
// Markdown constructs have no direct equivalent and are mapped to the closest permitted tag:
//
//	image      -> <a>, because img src accepts mxc:// URIs only
//	task list  -> <li> prefixed with ☐/☑, because <input> is not permitted
//	latex      -> <div data-mx-maths> / <span data-mx-maths>
//	~channel   -> left as plain text; channel targets are not resolvable here
//	:emoji:    -> left as plain text; not Markdown, and clients render shortcodes themselves
var (
	reInlineCode = regexp.MustCompile("`([^`\\n]+)`")
	reBoldStar   = regexp.MustCompile(`\*\*([^\*\n]+)\*\*`)
	reBoldUnder  = regexp.MustCompile(`__([^_\n]+)__`)
	reStrike     = regexp.MustCompile(`~~([^~\n]+)~~`)
	reItalicStar = regexp.MustCompile(`\*([^*\n]+)\*`)
	reItalicUnd  = regexp.MustCompile(`_([^_\n]+)_`)
	// reMatrixMention matches a full Matrix user ID (@localpart:domain) inside an
	// already-normalized message. Localpart grammar follows the Matrix spec.
	reMatrixMention = regexp.MustCompile(`@[a-zA-Z0-9._=+/-]+:[a-zA-Z0-9.-]+`)

	// Inline constructs. All of these run against already-escaped text, so a literal quote
	// from the source appears as &#34; and titles are matched accordingly.
	reLink        = regexp.MustCompile(`\[(.*?)\]\(\s*([^\s)]+?)` + reTitleSuffix + `\s*\)`)
	reImage       = regexp.MustCompile(`!\[([^\]]*)\]\(\s*([^\s)]+?)` + reTitleSuffix + `\s*\)`)
	reLinkedImage = regexp.MustCompile(`\[!\[([^\]]*)\]\([^)]*\)\]\(\s*([^\s)]+?)` + reTitleSuffix + `\s*\)`)
	reAutolink    = regexp.MustCompile(`(?:https?|ftp)://[^\s]+|www\.[^\s]+|[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)
	reInlineMath  = regexp.MustCompile(`\$([^$\n]+)\$`)
	reEscape      = regexp.MustCompile("\\\\([\\\\`*_{}\\[\\]()#+\\-.!|~$])")

	// Block-level constructs. html.EscapeString leaves #, -, *, _, +, =, |, : and [ untouched,
	// so matching these against escaped lines is safe.
	reHeading        = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	reBullet         = regexp.MustCompile(`^(\s*)[-*+](\s+.*)$`)
	reOrdered        = regexp.MustCompile(`^(\s*)(\d+)[.)](\s+.*)$`)
	reHorizRule      = regexp.MustCompile(`^\s*(-{3,}|\*{3,}|_{3,})\s*$`)
	reSetextH1       = regexp.MustCompile(`^\s*=+\s*$`)
	reSetextH2       = regexp.MustCompile(`^\s*-+\s*$`)
	reTaskItem       = regexp.MustCompile(`^\[([ xX])\]\s+(.*)$`)
	reTableDelimCell = regexp.MustCompile(`^:?-+:?$`)
)

// reTitleSuffix matches the optional title or =WxH size suffix Mattermost allows after a link
// or image target. Quotes are entity-encoded because it runs on escaped text.
const reTitleSuffix = `(?:\s+(?:=\d*x\d*|&#34;.*?&#34;|&#39;.*?&#39;))?`

// linkSchemes are the href schemes the Matrix spec permits. Anything else is left as plain
// text rather than turned into an anchor a client would strip.
var linkSchemes = []string{"https://", "http://", "ftp://", "mailto:", "magnet:", "matrix:"}

// renderMatrixMessageHTML converts a normalized message (mentions already written as
// @localpart:domain) into Matrix HTML. Mentions are masked out before the markdown pass so
// their underscores are never interpreted as emphasis, then spliced back in as matrix.to
// pills. It also returns the deduped list of mentioned user IDs for the m.mentions field.
func renderMatrixMessageHTML(message string) (string, []string) {
	if message == "" {
		return "", nil
	}

	var ids []string
	seen := make(map[string]struct{})
	pills := make(map[string]string)
	idx := 0

	masked := reMatrixMention.ReplaceAllStringFunc(message, func(m string) string {
		if _, ok := seen[m]; !ok {
			seen[m] = struct{}{}
			ids = append(ids, m)
		}
		// NUL-delimited placeholder: contains no markdown-significant characters and is
		// left untouched by html.EscapeString, so it survives the markdown pass intact.
		key := fmt.Sprintf("\x00MENTION%d\x00", idx)
		idx++
		pills[key] = fmt.Sprintf(`<a href="https://matrix.to/#/%s">%s</a>`, m, html.EscapeString(m))
		return key
	})

	out := markdownToMatrixHTML(masked)
	for key, pill := range pills {
		out = strings.ReplaceAll(out, key, pill)
	}
	return out, ids
}

// markdownToMatrixHTML converts a subset of Mattermost/Markdown syntax to Matrix HTML.
// It favors safe rendering with escaped fallback text instead of full Markdown compatibility.
func markdownToMatrixHTML(input string) string {
	if input == "" {
		return ""
	}

	parts := strings.Split(input, "```")
	var b strings.Builder

	for idx, part := range parts {
		// Odd segments are fenced code blocks.
		if idx%2 == 1 {
			lang := ""
			code := part
			if nl := strings.IndexByte(code, '\n'); nl >= 0 {
				lang = strings.TrimSpace(code[:nl])
				code = code[nl+1:]
			}

			// A latex/math fence becomes block maths with the source as fallback text.
			if lang == "latex" || lang == "math" {
				escaped := html.EscapeString(strings.TrimRight(code, "\n"))
				fmt.Fprintf(&b, `<div data-mx-maths="%s"><code>%s</code></div>`, escaped, escaped)
				continue
			}

			b.WriteString("<pre><code")
			if lang != "" {
				b.WriteString(` class="language-`)
				b.WriteString(html.EscapeString(lang))
				b.WriteString(`"`)
			}
			b.WriteString(">")
			b.WriteString(html.EscapeString(code))
			b.WriteString("</code></pre>")
			continue
		}

		segment := renderMarkdownSegment(part)
		b.WriteString(segment)
	}

	return b.String()
}

// renderMarkdownSegment escapes a non-code segment once, then renders it as a sequence of
// blocks. Escaping up front means every later pass works on safe text.
func renderMarkdownSegment(segment string) string {
	if segment == "" {
		return ""
	}

	return renderBlocks(strings.Split(html.EscapeString(segment), "\n"))
}

// chunk is one rendered piece of a segment. isBlock marks the block-level elements whose
// surrounding blank lines are Markdown separator syntax rather than content.
type chunk struct {
	html    string
	isBlock bool
}

// renderBlocks walks escaped lines, dispatching each to the block renderer that claims it.
// Chunks are joined with <br> so plain prose keeps its original line breaks.
func renderBlocks(lines []string) string {
	var chunks []chunk

	for i := 0; i < len(lines); {
		line := lines[i]

		// Tables are checked first: their delimiter row would otherwise read as a rule.
		if strings.Contains(line, "|") && i+1 < len(lines) && isTableDelimiter(lines[i+1]) &&
			len(splitTableRow(lines[i+1])) == len(splitTableRow(line)) {
			rendered, next := renderTable(lines, i)
			chunks = append(chunks, chunk{html: rendered, isBlock: true})
			i = next
			continue
		}

		if m := reHeading.FindStringSubmatch(line); m != nil {
			chunks = append(chunks, chunk{html: renderHeading(len(m[1]), m[2]), isBlock: true})
			i++
			continue
		}

		if reBullet.MatchString(line) || reOrdered.MatchString(line) {
			rendered, next := renderList(lines, i)
			chunks = append(chunks, chunk{html: rendered, isBlock: true})
			i = next
			continue
		}

		if strings.HasPrefix(line, "&gt;") {
			rendered, next := renderBlockquote(lines, i)
			chunks = append(chunks, chunk{html: rendered})
			i = next
			continue
		}

		if reHorizRule.MatchString(line) {
			chunks = append(chunks, chunk{html: "<hr>", isBlock: true})
			i++
			continue
		}

		// Setext heading: a paragraph line underlined with = or -. Checked last because an
		// ATX heading, list item, quote or rule on this line takes precedence.
		if strings.TrimSpace(line) != "" && i+1 < len(lines) {
			if reSetextH1.MatchString(lines[i+1]) {
				chunks = append(chunks, chunk{html: renderHeading(1, line), isBlock: true})
				i += 2
				continue
			}
			if reSetextH2.MatchString(lines[i+1]) {
				chunks = append(chunks, chunk{html: renderHeading(2, line), isBlock: true})
				i += 2
				continue
			}
		}

		chunks = append(chunks, chunk{html: renderInline(line)})
		i++
	}

	return joinChunks(chunks)
}

func joinChunks(chunks []chunk) string {
	var kept []string
	for i, c := range chunks {
		// A blank line next to a block element is separator syntax; keeping it would render
		// as a stray empty line above or below the block.
		if !c.isBlock && c.html == "" {
			prevBlock := i > 0 && chunks[i-1].isBlock
			nextBlock := i+1 < len(chunks) && chunks[i+1].isBlock
			if prevBlock || nextBlock {
				continue
			}
		}
		kept = append(kept, c.html)
	}
	return strings.Join(kept, "<br>")
}

func renderHeading(level int, text string) string {
	return fmt.Sprintf("<h%d>%s</h%d>", level, renderInline(strings.TrimSpace(text)), level)
}

// renderBlockquote consumes a run of quoted lines, strips one level of quoting, and renders
// the remainder as blocks. That gives nested quotes, and lists or tables inside a quote,
// without any extra handling.
func renderBlockquote(lines []string, start int) (string, int) {
	var inner []string
	i := start
	for ; i < len(lines) && strings.HasPrefix(lines[i], "&gt;"); i++ {
		// Strip the marker and at most one following space, so indentation of nested
		// constructs survives.
		inner = append(inner, strings.TrimPrefix(strings.TrimPrefix(lines[i], "&gt;"), " "))
	}
	return "<blockquote>" + renderBlocks(inner) + "</blockquote>", i
}

// listItem is one collected list line, before nesting is resolved.
type listItem struct {
	indent  int
	ordered bool
	num     int
	text    string
}

// renderList consumes a run of list lines and renders them as nested lists, returning the
// index of the first line after the run.
func renderList(lines []string, start int) (string, int) {
	var items []listItem
	i := start
	for ; i < len(lines); i++ {
		if reHorizRule.MatchString(lines[i]) {
			break
		}
		if m := reOrdered.FindStringSubmatch(lines[i]); m != nil {
			num, err := strconv.Atoi(m[2])
			if err != nil {
				break
			}
			items = append(items, listItem{
				indent:  indentWidth(m[1]),
				ordered: true,
				num:     num,
				text:    strings.TrimSpace(m[3]),
			})
			continue
		}
		if m := reBullet.FindStringSubmatch(lines[i]); m != nil {
			items = append(items, listItem{
				indent: indentWidth(m[1]),
				text:   strings.TrimSpace(m[2]),
			})
			continue
		}
		break
	}

	rendered, _ := renderListLevel(items, 0)
	return rendered, i
}

// renderListLevel renders items[pos:] that belong to the level of items[pos], recursing for
// any deeper-indented items. It returns the rendered list and the index it stopped at.
func renderListLevel(items []listItem, pos int) (string, int) {
	base := items[pos]

	var b strings.Builder
	switch {
	case base.ordered && base.num != 1:
		fmt.Fprintf(&b, `<ol start="%d">`, base.num)
	case base.ordered:
		b.WriteString("<ol>")
	default:
		b.WriteString("<ul>")
	}

	for pos < len(items) {
		item := items[pos]
		if item.indent < base.indent || (item.indent == base.indent && item.ordered != base.ordered) {
			break
		}
		if item.indent > base.indent {
			// Deeper item without a sibling above it at this level: still nest it.
			nested, next := renderListLevel(items, pos)
			b.WriteString(nested)
			pos = next
			continue
		}

		b.WriteString("<li>")
		b.WriteString(renderListItemText(item.text))
		pos++
		if pos < len(items) && items[pos].indent > base.indent {
			nested, next := renderListLevel(items, pos)
			b.WriteString(nested)
			pos = next
		}
		b.WriteString("</li>")
	}

	if base.ordered {
		b.WriteString("</ol>")
	} else {
		b.WriteString("</ul>")
	}
	return b.String(), pos
}

// renderListItemText renders item content, converting a task-list checkbox into a symbol
// because the Matrix spec permits no <input> element.
func renderListItemText(text string) string {
	if m := reTaskItem.FindStringSubmatch(text); m != nil {
		box := "☐ "
		if m[1] != " " {
			box = "☑ "
		}
		return box + renderInline(m[2])
	}
	return renderInline(text)
}

// indentWidth counts leading whitespace, treating a tab as four columns.
func indentWidth(prefix string) int {
	width := 0
	for _, r := range prefix {
		if r == '\t' {
			width += 4
			continue
		}
		width++
	}
	return width
}

// renderTable renders a GFM pipe table starting at the header row and returns the index of the
// first line after it. Alignment colons in the delimiter row are parsed and discarded: the
// Matrix spec permits no attributes on th/td, so clients strip align= anyway.
func renderTable(lines []string, start int) (string, int) {
	header := splitTableRow(lines[start])
	cols := len(header)

	var b strings.Builder
	b.WriteString("<table><thead><tr>")
	for _, cell := range header {
		b.WriteString("<th>")
		b.WriteString(renderInline(cell))
		b.WriteString("</th>")
	}
	b.WriteString("</tr></thead>")

	var body strings.Builder
	i := start + 2 // skip the header and delimiter rows
	for ; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" || !strings.Contains(line, "|") {
			break
		}
		cells := splitTableRow(line)
		body.WriteString("<tr>")
		for c := 0; c < cols; c++ {
			cell := ""
			if c < len(cells) {
				cell = cells[c]
			}
			body.WriteString("<td>")
			body.WriteString(renderInline(cell))
			body.WriteString("</td>")
		}
		body.WriteString("</tr>")
	}
	if body.Len() > 0 {
		b.WriteString("<tbody>")
		b.WriteString(body.String())
		b.WriteString("</tbody>")
	}
	b.WriteString("</table>")

	return b.String(), i
}

// isTableDelimiter reports whether a line is a table's ---|---|--- separator row.
func isTableDelimiter(line string) bool {
	if !strings.Contains(line, "-") {
		return false
	}
	cells := splitTableRow(line)
	if len(cells) == 0 {
		return false
	}
	for _, cell := range cells {
		if !reTableDelimCell.MatchString(cell) {
			return false
		}
	}
	return true
}

// splitTableRow splits a table row into trimmed cells. Outer pipes are optional, and \| is a
// literal pipe rather than a cell break.
func splitTableRow(line string) []string {
	s := strings.TrimSpace(line)

	var cells []string
	var cur strings.Builder
	escaped := false
	for _, r := range s {
		switch {
		case escaped:
			if r != '|' {
				cur.WriteRune('\\')
			}
			cur.WriteRune(r)
			escaped = false
		case r == '\\':
			escaped = true
		case r == '|':
			cells = append(cells, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if escaped {
		cur.WriteRune('\\')
	}
	cells = append(cells, cur.String())

	// A leading or trailing pipe yields an empty outer cell that is punctuation, not data.
	if len(cells) > 1 && strings.TrimSpace(cells[0]) == "" && strings.HasPrefix(s, "|") {
		cells = cells[1:]
	}
	if len(cells) > 1 && strings.TrimSpace(cells[len(cells)-1]) == "" && strings.HasSuffix(s, "|") {
		cells = cells[:len(cells)-1]
	}

	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells
}

// masker parks finished HTML behind NUL-delimited placeholders so later passes cannot reach
// inside it. The placeholders hold only digits, so no markdown or escaping pass touches them.
type masker struct {
	tokens []string
}

func (m *masker) mask(rendered string) string {
	m.tokens = append(m.tokens, rendered)
	return fmt.Sprintf("\x00I%d\x00", len(m.tokens)-1)
}

// restore expands placeholders newest-first, since a token can only ever contain placeholders
// created before it.
func (m *masker) restore(s string) string {
	for i := len(m.tokens) - 1; i >= 0; i-- {
		s = strings.ReplaceAll(s, fmt.Sprintf("\x00I%d\x00", i), m.tokens[i])
	}
	return s
}

// renderInline applies the inline markdown passes to a single line's worth of escaped text.
// Code, links, images, maths and backslash escapes are rendered and masked first so that
// emphasis cannot reach inside them, then emphasis runs, then the masks are expanded.
func renderInline(text string) string {
	return renderInlineDepth(text, 0)
}

func renderInlineDepth(text string, depth int) string {
	if text == "" {
		return ""
	}

	m := &masker{}
	out := reInlineCode.ReplaceAllStringFunc(text, func(s string) string {
		return m.mask("<code>" + reInlineCode.FindStringSubmatch(s)[1] + "</code>")
	})
	out = maskMath(m, out)
	out = maskImages(m, out, depth)
	out = maskLinks(m, out, depth)
	out = maskAutolinks(m, out)
	out = maskEscapes(m, out)

	out = reBoldStar.ReplaceAllString(out, "<strong>$1</strong>")
	out = reBoldUnder.ReplaceAllString(out, "<strong>$1</strong>")
	out = reStrike.ReplaceAllString(out, "<del>$1</del>")
	out = reItalicStar.ReplaceAllString(out, "<em>$1</em>")
	out = reItalicUnd.ReplaceAllString(out, "<em>$1</em>")

	return m.restore(out)
}

// maskMath renders $...$ as inline maths. It deliberately requires a LaTeX-looking body: plain
// prose about money ("$5 and $10") must not be swallowed.
func maskMath(m *masker, text string) string {
	return reInlineMath.ReplaceAllStringFunc(text, func(s string) string {
		body := reInlineMath.FindStringSubmatch(s)[1]
		if body != strings.TrimSpace(body) || !strings.ContainsAny(body, `\^_{}`) {
			return s
		}
		return m.mask(fmt.Sprintf(`<span data-mx-maths="%s"><code>%s</code></span>`, body, body))
	})
}

// maskImages renders images as links, because the Matrix spec only permits mxc:// in img src.
// A markdown-linked image collapses to its outer link so no anchor ends up nested.
func maskImages(m *masker, text string, depth int) string {
	out := reLinkedImage.ReplaceAllStringFunc(text, func(s string) string {
		g := reLinkedImage.FindStringSubmatch(s)
		return m.mask(imageHTML(g[2], g[1], depth))
	})
	return reImage.ReplaceAllStringFunc(out, func(s string) string {
		g := reImage.FindStringSubmatch(s)
		return m.mask(imageHTML(g[2], g[1], depth))
	})
}

// imageHTML renders one image reference. An mxc:// target is a real image; anything else can
// only become a link, since the spec rejects other schemes in img src.
func imageHTML(target, alt string, depth int) string {
	label := strings.TrimSpace(alt)
	if label == "" {
		label = target
	}
	if strings.HasPrefix(strings.ToLower(target), "mxc://") {
		return fmt.Sprintf(`<img src="%s" alt="%s">`, target, label)
	}
	if href, ok := safeHref(target); ok {
		return fmt.Sprintf(`<a href="%s">%s</a>`, href, renderLabel(label, depth))
	}
	return renderLabel(label, depth)
}

// maskLinks renders markdown links whose scheme the Matrix spec permits, and leaves any other
// target as plain text rather than emitting an anchor clients would strip.
func maskLinks(m *masker, text string, depth int) string {
	return reLink.ReplaceAllStringFunc(text, func(s string) string {
		g := reLink.FindStringSubmatch(s)
		href, ok := safeHref(g[2])
		if !ok {
			return s
		}
		label := g[1]
		if strings.TrimSpace(label) == "" {
			label = g[2]
		}
		return m.mask(fmt.Sprintf(`<a href="%s">%s</a>`, href, renderLabel(label, depth)))
	})
}

// maskAutolinks links bare URLs and email addresses, the way Mattermost does when rendering.
func maskAutolinks(m *masker, text string) string {
	return reAutolink.ReplaceAllStringFunc(text, func(s string) string {
		target, trailing := trimURLTail(s)
		href := target
		switch {
		case strings.HasPrefix(target, "www."):
			href = "https://" + target
		case !hasScheme(target) && strings.Contains(target, "@"):
			href = "mailto:" + target
		}
		if _, ok := safeHref(href); !ok {
			return s
		}
		return m.mask(fmt.Sprintf(`<a href="%s">%s</a>`, href, target)) + trailing
	})
}

// maskEscapes turns a backslash-escaped punctuation character into a literal, masking it so no
// later pass reads it as markup.
func maskEscapes(m *masker, text string) string {
	return reEscape.ReplaceAllStringFunc(text, func(s string) string {
		return m.mask(reEscape.FindStringSubmatch(s)[1])
	})
}

// renderLabel renders link or image label text, with a depth guard against pathological nesting.
func renderLabel(label string, depth int) string {
	if depth >= 4 {
		return label
	}
	return renderInlineDepth(label, depth+1)
}

// safeHref reports whether a target uses a scheme the Matrix spec permits in href.
func safeHref(target string) (string, bool) {
	if !hasScheme(target) {
		return "", false
	}
	return target, true
}

func hasScheme(target string) bool {
	lower := strings.ToLower(target)
	for _, scheme := range linkSchemes {
		if strings.HasPrefix(lower, scheme) {
			return true
		}
	}
	return false
}

// trimURLTail splits sentence punctuation and entity boundaries off the end of an autolinked
// URL, returning the URL and the text that follows it.
func trimURLTail(s string) (string, string) {
	url := s
	for {
		trimmed := strings.TrimRight(url, ".,;:!?")
		for _, entity := range []string{"&gt;", "&lt;", "&quot;", "&#34;", "&#39;"} {
			trimmed = strings.TrimSuffix(trimmed, entity)
		}
		if strings.HasSuffix(trimmed, ")") && strings.Count(trimmed, ")") > strings.Count(trimmed, "(") {
			trimmed = strings.TrimSuffix(trimmed, ")")
		}
		if trimmed == url {
			return url, s[len(url):]
		}
		url = trimmed
	}
}
