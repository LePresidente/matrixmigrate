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

