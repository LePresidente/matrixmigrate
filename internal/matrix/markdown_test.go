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
			name: "heading",
			in:   "## Heading\ntext",
			want: "<h2>Heading</h2><br>text",
		},
		{
			name: "horizontal rule",
			in:   "above\n\n---\n\nbelow",
			want: "above<br><hr><br>below",
		},
		{
			name: "consecutive quote lines merge",
			in:   "> one\n> two",
			want: "<blockquote>one<br>two</blockquote>",
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
