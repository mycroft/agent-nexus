package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

const (
	maxPageBytes = 4 << 20
	pageTTL      = 15 * time.Minute
	maxPages     = 32
)

var (
	githubBlob = regexp.MustCompile(`^https://github\.com/([^/]+)/([^/]+)/blob/(.+)$`)
	blankLines = regexp.MustCompile(`\n{3,}`)
)

// Page is a fetched document as text.
type Page struct {
	// URL is where the content came from, after rewrites and redirects.
	URL   string
	Title string
	Text  string
}

type cachedPage struct {
	page    *Page
	expires time.Time
}

// FetchPage reads a page as text: HTML converted, markdown, plain text,
// JSON and YAML as they are. A GitHub file page is read from its raw
// content instead of the rendered HTML around it.
func (c *Client) FetchPage(ctx context.Context, rawURL string) (*Page, error) {
	target := rawURL
	if m := githubBlob.FindStringSubmatch(rawURL); m != nil {
		target = c.rawGitHub + "/" + m[1] + "/" + m[2] + "/" + m[3]
	}
	c.mu.Lock()
	cached, ok := c.pages[target]
	c.mu.Unlock()
	if ok && c.now().Before(cached.expires) {
		return cached.page, nil
	}
	resp, err := c.Get(ctx, Request{
		URL:    target,
		Accept: "text/html, text/markdown;q=0.9, text/plain;q=0.8, application/json;q=0.5, */*;q=0.1",
		Limit:  maxPageBytes,
	})
	if err != nil {
		return nil, err
	}
	page := &Page{URL: resp.URL}
	mediaType, _, _ := mime.ParseMediaType(resp.ContentType)
	switch {
	case mediaType == "text/html" || mediaType == "application/xhtml+xml":
		page.Title, page.Text = htmlToText(resp.Body)
	case strings.HasPrefix(mediaType, "text/") || mediaType == "application/json" || strings.HasSuffix(mediaType, "+json") ||
		strings.Contains(mediaType, "yaml") || mediaType == "":
		if bytes.IndexByte(resp.Body, 0) >= 0 {
			return nil, fmt.Errorf("%s is binary", Redact(resp.URL))
		}
		page.Text = string(resp.Body)
	default:
		return nil, fmt.Errorf("%s is %s, not a text page", Redact(resp.URL), mediaType)
	}
	c.mu.Lock()
	if len(c.pages) >= maxPages {
		for k, v := range c.pages {
			if c.now().After(v.expires) || len(c.pages) >= maxPages {
				delete(c.pages, k)
			}
		}
	}
	c.pages[target] = cachedPage{page: page, expires: c.now().Add(pageTTL)}
	c.mu.Unlock()
	return page, nil
}

// skipped elements hold no reading content.
var skipped = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Noscript: true, atom.Svg: true, atom.Template: true,
	atom.Iframe: true, atom.Nav: true, atom.Footer: true, atom.Form: true, atom.Button: true, atom.Head: true,
}

// blocks start on a line of their own.
var blocks = map[atom.Atom]bool{
	atom.P: true, atom.Div: true, atom.Section: true, atom.Article: true, atom.Main: true, atom.Header: true,
	atom.Aside: true, atom.Blockquote: true, atom.Ul: true, atom.Ol: true, atom.Li: true, atom.Table: true,
	atom.Tr: true, atom.Pre: true, atom.Br: true, atom.Hr: true, atom.Dl: true, atom.Dt: true, atom.Dd: true,
	atom.Details: true, atom.Summary: true, atom.Figure: true, atom.H1: true, atom.H2: true, atom.H3: true,
	atom.H4: true, atom.H5: true, atom.H6: true,
}

var headings = map[atom.Atom]string{
	atom.H1: "# ", atom.H2: "## ", atom.H3: "### ", atom.H4: "#### ", atom.H5: "##### ", atom.H6: "###### ",
}

// htmlToText renders an HTML document as markdown-like text: headings,
// list items, code blocks and table cells kept, navigation and scripts
// dropped. It returns the title and the text.
func htmlToText(src []byte) (string, string) {
	doc, err := html.Parse(bytes.NewReader(src))
	if err != nil {
		return "", string(src)
	}
	r := &renderer{}
	r.node(doc, false)
	lines := strings.Split(r.b.String(), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	text := blankLines.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
	return r.title, strings.TrimSpace(text) + "\n"
}

// renderer accumulates the text of an HTML tree.
type renderer struct {
	b     strings.Builder
	title string
}

func (r *renderer) node(n *html.Node, pre bool) {
	switch n.Type {
	case html.TextNode:
		r.text(n.Data, pre)
		return
	case html.ElementNode:
		if n.DataAtom == atom.Head {
			r.findTitle(n)
		}
		if skipped[n.DataAtom] {
			return
		}
	case html.DocumentNode:
	default:
		return
	}
	r.open(n.DataAtom, pre)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		r.node(c, pre || n.DataAtom == atom.Pre)
	}
	r.close(n.DataAtom, pre)
}

func (r *renderer) findTitle(head *html.Node) {
	for c := head.FirstChild; c != nil; c = c.NextSibling {
		if c.DataAtom == atom.Title && c.FirstChild != nil && r.title == "" {
			r.title = strings.TrimSpace(c.FirstChild.Data)
		}
	}
}

// text writes a text node: verbatim in a code block, else with its
// whitespace collapsed to single spaces.
func (r *renderer) text(data string, pre bool) {
	if pre {
		r.b.WriteString(data)
		return
	}
	words := strings.Fields(data)
	if len(words) == 0 {
		if data != "" {
			r.space()
		}
		return
	}
	if first, _ := utf8.DecodeRuneInString(data); unicode.IsSpace(first) {
		r.space()
	}
	r.b.WriteString(strings.Join(words, " "))
	if last, _ := utf8.DecodeLastRuneInString(data); unicode.IsSpace(last) {
		r.space()
	}
}

func (r *renderer) open(a atom.Atom, pre bool) {
	switch {
	case a == atom.Pre:
		r.newline()
		r.b.WriteString("```\n")
	case blocks[a]:
		r.newline()
		if h, ok := headings[a]; ok {
			r.b.WriteString("\n" + h)
		}
		if a == atom.Li {
			r.b.WriteString("- ")
		}
	case a == atom.Code && !pre:
		r.b.WriteByte('`')
	case a == atom.Td || a == atom.Th:
		r.b.WriteString(" | ")
	}
}

func (r *renderer) close(a atom.Atom, pre bool) {
	switch {
	case a == atom.Pre:
		r.newline()
		r.b.WriteString("```\n")
	case a == atom.Code && !pre:
		r.b.WriteByte('`')
	case blocks[a]:
		r.newline()
	}
}

// newline ends the current line, unless it is empty or holds only a list
// marker, which keeps "- " on the line of the item's first block.
func (r *renderer) newline() {
	s := r.b.String()
	if s == "" || strings.HasSuffix(s, "\n") || s == "- " || strings.HasSuffix(s, "\n- ") {
		return
	}
	r.b.WriteByte('\n')
}

func (r *renderer) space() {
	if s := r.b.String(); s != "" && !strings.HasSuffix(s, " ") && !strings.HasSuffix(s, "\n") {
		r.b.WriteByte(' ')
	}
}

// PageHost returns the host of a page URL the model may request: https
// only, no credentials or query, at most 512 characters.
func PageHost(rawURL string) (string, error) {
	if len(rawURL) > 512 {
		return "", errors.New("URL longer than 512 characters")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("invalid URL: %w", err)
	}
	switch {
	case u.Scheme != "https":
		return "", fmt.Errorf("URL %q: want https://", rawURL)
	case u.Host == "" || u.User != nil || u.Opaque != "":
		return "", fmt.Errorf("URL %q: want https://host/path", rawURL)
	case u.RawQuery != "" || u.ForceQuery:
		return "", fmt.Errorf("URL %q: query strings are not allowed", rawURL)
	}
	return strings.ToLower(u.Hostname()), nil
}
