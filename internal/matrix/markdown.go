package matrix

import (
	"fmt"
	"html"
	"regexp"
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

func renderMarkdownSegment(segment string) string {
	if segment == "" {
		return ""
	}

	escaped := html.EscapeString(segment)
	lines := strings.Split(escaped, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "&gt;") {
			lines[i] = "<blockquote>" + strings.TrimSpace(strings.TrimPrefix(line, "&gt;")) + "</blockquote>"
		}
	}
	out := strings.Join(lines, "\n")

	out = reInlineCode.ReplaceAllString(out, "<code>$1</code>")
	out = reLink.ReplaceAllString(out, `<a href="$2">$1</a>`)
	out = reBoldStar.ReplaceAllString(out, "<strong>$1</strong>")
	out = reBoldUnder.ReplaceAllString(out, "<strong>$1</strong>")
	out = reStrike.ReplaceAllString(out, "<del>$1</del>")
	out = reItalicStar.ReplaceAllString(out, "<em>$1</em>")
	out = reItalicUnd.ReplaceAllString(out, "<em>$1</em>")

	out = strings.ReplaceAll(out, "\n", "<br>")
	return out
}

