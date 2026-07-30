package matrix

import (
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
)

var (
	reInlineCode = regexp.MustCompile("`([^`\\n]+)`")
	reBoldStar   = regexp.MustCompile(`\*\*([^\*\n]+)\*\*`)
	reBoldUnder  = regexp.MustCompile(`__([^_\n]+)__`)
	reStrike     = regexp.MustCompile(`~~([^~\n]+)~~`)
	reItalicStar = regexp.MustCompile(`\*([^*\n]+)\*`)
	reItalicUnd  = regexp.MustCompile(`_([^_\n]+)_`)
	reLink       = regexp.MustCompile(`\[(.*?)\]\((https?://[^\s)]+)\)`)
	// reMatrixMention matches a full Matrix user ID (@localpart:domain) inside an
	// already-normalized message. Localpart grammar follows the Matrix spec.
	reMatrixMention = regexp.MustCompile(`@[a-zA-Z0-9._=+/-]+:[a-zA-Z0-9.-]+`)

	// Block-level constructs. All are matched against already-escaped lines, which is safe
	// because html.EscapeString leaves #, -, *, _, +, | and : untouched.
	reHeading        = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	reBullet         = regexp.MustCompile(`^\s*[-*+]\s+(.*)$`)
	reOrdered        = regexp.MustCompile(`^\s*(\d+)[.)]\s+(.*)$`)
	reHorizRule      = regexp.MustCompile(`^\s*(-{3,}|\*{3,}|_{3,})\s*$`)
	reTableDelimCell = regexp.MustCompile(`^:?-+:?$`)
)

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
			level := len(m[1])
			chunks = append(chunks, chunk{
				html:    fmt.Sprintf("<h%d>%s</h%d>", level, renderInline(m[2]), level),
				isBlock: true,
			})
			i++
			continue
		}

		if reHorizRule.MatchString(line) {
			chunks = append(chunks, chunk{html: "<hr>", isBlock: true})
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

// renderInline applies the inline markdown passes to a single line's worth of escaped text.
// Order matters: code and links first so their contents are not re-processed as emphasis,
// and bold before italic so ** is not consumed by the single-* rule.
func renderInline(text string) string {
	out := reInlineCode.ReplaceAllString(text, "<code>$1</code>")
	out = reLink.ReplaceAllString(out, `<a href="$2">$1</a>`)
	out = reBoldStar.ReplaceAllString(out, "<strong>$1</strong>")
	out = reBoldUnder.ReplaceAllString(out, "<strong>$1</strong>")
	out = reStrike.ReplaceAllString(out, "<del>$1</del>")
	out = reItalicStar.ReplaceAllString(out, "<em>$1</em>")
	out = reItalicUnd.ReplaceAllString(out, "<em>$1</em>")
	return out
}

// renderBlockquote merges consecutive quoted lines into one blockquote and returns the index
// of the first line after it.
func renderBlockquote(lines []string, start int) (string, int) {
	var quoted []string
	i := start
	for ; i < len(lines) && strings.HasPrefix(lines[i], "&gt;"); i++ {
		quoted = append(quoted, renderInline(strings.TrimSpace(strings.TrimPrefix(lines[i], "&gt;"))))
	}
	return "<blockquote>" + strings.Join(quoted, "<br>") + "</blockquote>", i
}

// renderList consumes a run of list items of one kind (bulleted or numbered) and returns the
// index of the first line after it. Nested indentation is flattened into a single level.
func renderList(lines []string, start int) (string, int) {
	ordered := false
	startNum := 1
	if m := reOrdered.FindStringSubmatch(lines[start]); m != nil {
		ordered = true
		if n, err := strconv.Atoi(m[1]); err == nil {
			startNum = n
		}
	}

	var items []string
	i := start
	for ; i < len(lines); i++ {
		if reHorizRule.MatchString(lines[i]) {
			break
		}
		if m := reOrdered.FindStringSubmatch(lines[i]); m != nil {
			if !ordered {
				break
			}
			items = append(items, m[2])
			continue
		}
		if m := reBullet.FindStringSubmatch(lines[i]); m != nil {
			if ordered {
				break
			}
			items = append(items, m[1])
			continue
		}
		break
	}

	var b strings.Builder
	switch {
	case ordered && startNum != 1:
		fmt.Fprintf(&b, `<ol start="%d">`, startNum)
	case ordered:
		b.WriteString("<ol>")
	default:
		b.WriteString("<ul>")
	}
	for _, item := range items {
		b.WriteString("<li>")
		b.WriteString(renderInline(strings.TrimSpace(item)))
		b.WriteString("</li>")
	}
	if ordered {
		b.WriteString("</ol>")
	} else {
		b.WriteString("</ul>")
	}
	return b.String(), i
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
