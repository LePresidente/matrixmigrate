package matrix

import "testing"

func TestMarkdownToMatrixHTML(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "bold italic link and inline code",
			in:   "**bold** _italic_ [site](https://example.com) `code`",
			want: "<strong>bold</strong> <em>italic</em> <a href=\"https://example.com\">site</a> <code>code</code>",
		},
		{
			name: "fenced code block",
			in:   "```go\nfmt.Println(\"hi\")\n```",
			want: "<pre><code class=\"language-go\">fmt.Println(&#34;hi&#34;)\n</code></pre>",
		},
		{
			name: "blockquote and newline",
			in:   "> quoted\nnext",
			want: "<blockquote>quoted</blockquote><br>next",
		},
		{
			name: "bold italic combined",
			in:   "***both***",
			want: "<em><strong>both</strong></em>",
		},
		{
			name: "strikethrough",
			in:   "~~gone~~",
			want: "<del>gone</del>",
		},

		// Tables
		{
			name: "table between prose lines",
			in:   "GW 7 Summary\n\n| Name | Rank |\n| --- | --- |\n| alice | 1 |\n| bob_dev | 2 |\n\nGW 8 Summary",
			want: "GW 7 Summary<br>" +
				"<table><thead><tr><th>Name</th><th>Rank</th></tr></thead>" +
				"<tbody><tr><td>alice</td><td>1</td></tr><tr><td>bob_dev</td><td>2</td></tr></tbody></table>" +
				"<br>GW 8 Summary",
		},
		{
			name: "table without outer pipes",
			in:   "Name | Rank\n--- | ---\nalice | 1",
			want: "<table><thead><tr><th>Name</th><th>Rank</th></tr></thead>" +
				"<tbody><tr><td>alice</td><td>1</td></tr></tbody></table>",
		},
		{
			name: "alignment colons parsed but not emitted",
			in:   "| a | b | c |\n| :--- | ---: | :---: |\n| 1 | 2 | 3 |",
			want: "<table><thead><tr><th>a</th><th>b</th><th>c</th></tr></thead>" +
				"<tbody><tr><td>1</td><td>2</td><td>3</td></tr></tbody></table>",
		},
		{
			name: "empty and missing cells are padded",
			in:   "| a | b | c |\n| --- | --- | --- |\n| 1 |  |",
			want: "<table><thead><tr><th>a</th><th>b</th><th>c</th></tr></thead>" +
				"<tbody><tr><td>1</td><td></td><td></td></tr></tbody></table>",
		},
		{
			name: "inline markdown inside cells",
			in:   "| **bold** |\n| --- |\n| _italic_ |",
			want: "<table><thead><tr><th><strong>bold</strong></th></tr></thead>" +
				"<tbody><tr><td><em>italic</em></td></tr></tbody></table>",
		},
		{
			name: "escaped pipe stays inside a cell",
			in:   "| a | b |\n| --- | --- |\n| x \\| y | z |",
			want: "<table><thead><tr><th>a</th><th>b</th></tr></thead>" +
				"<tbody><tr><td>x | y</td><td>z</td></tr></tbody></table>",
		},
		{
			name: "header only table has no tbody",
			in:   "| a | b |\n| --- | --- |",
			want: "<table><thead><tr><th>a</th><th>b</th></tr></thead></table>",
		},
		{
			name: "pipes without a delimiter row stay literal",
			in:   "alice | bob_dev\ncarol | dave",
			want: "alice | bob_dev<br>carol | dave",
		},

		// Headings and rules
		{
			name: "atx heading",
			in:   "## Heading\ntext",
			want: "<h2>Heading</h2><br>text",
		},
		{
			name: "setext heading level one",
			in:   "Title\n===\ntext",
			want: "<h1>Title</h1><br>text",
		},
		{
			name: "setext heading level two",
			in:   "Title\n---\ntext",
			want: "<h2>Title</h2><br>text",
		},
		{
			name: "horizontal rule",
			in:   "above\n\n---\n\nbelow",
			want: "above<br><hr><br>below",
		},

		// Lists
		{
			name: "unordered list",
			in:   "- one\n- two",
			want: "<ul><li>one</li><li>two</li></ul>",
		},
		{
			name: "ordered list",
			in:   "1. one\n2. two",
			want: "<ol><li>one</li><li>two</li></ol>",
		},
		{
			name: "ordered list with offset start",
			in:   "3. one\n4. two",
			want: "<ol start=\"3\"><li>one</li><li>two</li></ol>",
		},
		{
			name: "nested unordered list",
			in:   "- a\n  - b\n  - c\n- d",
			want: "<ul><li>a<ul><li>b</li><li>c</li></ul></li><li>d</li></ul>",
		},
		{
			name: "nested ordered list",
			in:   "1. a\n  1. b\n2. c",
			want: "<ol><li>a<ol><li>b</li></ol></li><li>c</li></ol>",
		},
		{
			name: "task list becomes checkbox symbols",
			in:   "- [ ] todo\n- [x] done",
			want: "<ul><li>☐ todo</li><li>☑ done</li></ul>",
		},

		// Quotes
		{
			name: "consecutive quote lines merge",
			in:   "> one\n> two",
			want: "<blockquote>one<br>two</blockquote>",
		},
		{
			name: "nested quote",
			in:   "> outer\n> > inner\n> back",
			want: "<blockquote>outer<br><blockquote>inner</blockquote><br>back</blockquote>",
		},
		{
			name: "list inside quote",
			in:   "> - a\n> - b",
			want: "<blockquote><ul><li>a</li><li>b</li></ul></blockquote>",
		},

		// Links and images
		{
			name: "image becomes a link because img src must be mxc",
			in:   "![alt](https://example.com/x.png)",
			want: "<a href=\"https://example.com/x.png\">alt</a>",
		},
		{
			name: "image title suffix is dropped",
			in:   "![alt](https://example.com/x.png \"hover\")",
			want: "<a href=\"https://example.com/x.png\">alt</a>",
		},
		{
			name: "image size suffix is dropped",
			in:   "![alt](https://example.com/x.png =50x40)",
			want: "<a href=\"https://example.com/x.png\">alt</a>",
		},
		{
			name: "mxc image stays an image",
			in:   "![logo](mxc://example.com/abc)",
			want: "<img src=\"mxc://example.com/abc\" alt=\"logo\">",
		},
		{
			name: "linked image collapses to the outer link",
			in:   "[![alt](https://example.com/x.png)](https://example.com/page)",
			want: "<a href=\"https://example.com/page\">alt</a>",
		},
		{
			name: "bare url is autolinked",
			in:   "see https://example.com/a?x=1&y=2 now",
			want: "see <a href=\"https://example.com/a?x=1&amp;y=2\">https://example.com/a?x=1&amp;y=2</a> now",
		},
		{
			name: "www url is autolinked over https",
			in:   "visit www.example.com.",
			want: "visit <a href=\"https://www.example.com\">www.example.com</a>.",
		},
		{
			name: "trailing paren is not part of the url",
			in:   "(see https://example.com/a)",
			want: "(see <a href=\"https://example.com/a\">https://example.com/a</a>)",
		},
		{
			name: "email is autolinked as mailto",
			in:   "mail alice@example.com please",
			want: "mail <a href=\"mailto:alice@example.com\">alice@example.com</a> please",
		},
		{
			name: "disallowed scheme is left as text",
			in:   "[bad](javascript:alert(1))",
			want: "[bad](javascript:alert(1))",
		},
		{
			name: "relative target is left as text",
			in:   "[rel](/local/path)",
			want: "[rel](/local/path)",
		},
		{
			name: "emphasis inside link label",
			in:   "**bold [link](https://example.com) in bold**",
			want: "<strong>bold <a href=\"https://example.com\">link</a> in bold</strong>",
		},

		// Escapes, maths, and things deliberately left alone
		{
			name: "backslash escapes suppress emphasis",
			in:   "\\*not bold\\* \\_keep\\_",
			want: "*not bold* _keep_",
		},
		{
			name: "emphasis does not reach inside code",
			in:   "`a_b_c` and _real_",
			want: "<code>a_b_c</code> and <em>real</em>",
		},
		{
			name: "money is not treated as maths",
			in:   "cost $5 and $10",
			want: "cost $5 and $10",
		},
		{
			name: "inline latex becomes data-mx-maths",
			in:   "math $\\frac{1}{2}$ here",
			want: "math <span data-mx-maths=\"\\frac{1}{2}\"><code>\\frac{1}{2}</code></span> here",
		},
		{
			name: "latex fence becomes block maths",
			in:   "```latex\n\\frac{1}{2}\n```",
			want: "<div data-mx-maths=\"\\frac{1}{2}\"><code>\\frac{1}{2}</code></div>",
		},
		{
			name: "channel links are left alone",
			in:   "~channel-name stays",
			want: "~channel-name stays",
		},

		// Reference links
		{
			name: "full reference link",
			in:   "see [site][ref] now\n\n[ref]: https://example.com",
			want: "see <a href=\"https://example.com\">site</a> now",
		},
		{
			name: "collapsed reference link",
			in:   "see [site][] now\n\n[site]: https://example.com",
			want: "see <a href=\"https://example.com\">site</a> now",
		},
		{
			name: "shortcut reference link",
			in:   "see [ref] now\n\n[ref]: https://example.com",
			want: "see <a href=\"https://example.com\">ref</a> now",
		},
		{
			name: "reference image",
			in:   "![alt][img]\n\n[img]: https://example.com/x.png",
			want: "<a href=\"https://example.com/x.png\">alt</a>",
		},
		{
			name: "unknown reference label stays literal",
			in:   "[unknown][nope] stays",
			want: "[unknown][nope] stays",
		},
		{
			name: "bracketed prose is not a reference link",
			in:   "prose [in brackets] stays",
			want: "prose [in brackets] stays",
		},
		{
			name: "inline link next to a defined label is untouched",
			in:   "[a](https://example.com/1) and [a][ref]\n\n[ref]: https://example.com/2",
			want: "<a href=\"https://example.com/1\">a</a> and <a href=\"https://example.com/2\">a</a>",
		},

		// Indented code
		{
			name: "indented code block",
			in:   "text\n\n    code line 1\n    code line 2\n\nafter",
			want: "text<br><pre><code>code line 1\ncode line 2</code></pre><br>after",
		},
		{
			name: "tab indented code block",
			in:   "\tcode",
			want: "<pre><code>code</code></pre>",
		},
		{
			name: "indented line without a blank line above is not code",
			in:   "para\n    not code",
			want: "para<br>    not code",
		},
		{
			name: "indented continuation under a list is not code",
			in:   "- a\n    continued",
			want: "<ul><li>a</li></ul><br>    continued",
		},

		// Emoji shortcodes
		{
			name: "known shortcodes expand and unknown ones stay",
			in:   "nice :smile: and :+1: and :custom_thing:",
			want: "nice 😄 and 👍 and :custom_thing:",
		},
		{
			name: "adjacent shortcodes",
			in:   ":smile::+1:",
			want: "😄👍",
		},
		{
			name: "shortcode inside code is left alone",
			in:   "`:smile:` stays",
			want: "<code>:smile:</code> stays",
		},
		{
			name: "colons in a url are not a shortcode",
			in:   "https://example.com/a:b:c",
			want: "<a href=\"https://example.com/a:b:c\">https://example.com/a:b:c</a>",
		},
		{
			name: "shortcode inside emphasis",
			in:   "**:tada: done**",
			want: "<strong>🎉 done</strong>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := markdownToMatrixHTML(tt.in)
			if got != tt.want {
				t.Fatalf("markdownToMatrixHTML() = %q, want %q", got, tt.want)
			}
		})
	}
}
