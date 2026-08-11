// Package markdown converts feed HTML into conservative, Obsidian-compatible
// Markdown.
//
// The tree is built from a raw token stream rather than a conforming HTML5
// parser on purpose: Herald applies a small, predictable set of auto-closing
// rules (li, p, tr, td/th) so that note output stays stable and reviewable.
// A conforming parser would additionally apply foster parenting and implied
// tags, silently reshaping author markup.
package markdown

import (
	"regexp"
	"strings"

	"github.com/PabloMoralesEscandon/herald/internal/textx"
	"golang.org/x/net/html"
)

var htmlTagPattern = regexp.MustCompile(
	`(?i)</?(?:a|article|aside|b|blockquote|br|code|dd|del|details|div|dl|dt|em|` +
		`embed|figcaption|figure|footer|h[1-6]|header|hr|i|iframe|img|kbd|li|main|mark|nav|object|ol|` +
		`p|pre|q|s|script|section|small|span|strike|strong|style|sub|summary|sup|svg|` +
		`table|tbody|td|template|tfoot|th|thead|time|tr|u|ul)\b`)

var blockTags = map[string]bool{
	"article": true, "aside": true, "dd": true, "details": true, "div": true,
	"dl": true, "dt": true, "figcaption": true, "figure": true, "footer": true,
	"header": true, "main": true, "nav": true, "p": true, "section": true,
	"summary": true,
}

var ignoredTags = map[string]bool{
	"embed": true, "iframe": true, "object": true, "script": true,
	"style": true, "svg": true, "template": true,
}

// voidTags are never pushed onto the open-element stack.
var voidTags = map[string]bool{
	"br": true, "hr": true, "img": true, "input": true, "meta": true, "link": true,
}

var (
	trailingSpace = regexp.MustCompile(`[ \t]+\n`)
	blankRun      = regexp.MustCompile(`\n{3,}`)
	spaceAroundNL = regexp.MustCompile(textx.SpaceClass + `*\n` + textx.SpaceClass + `*`)
)

// node is either an element (tag set) or a text leaf (text set, tag empty).
type node struct {
	tag      string
	attrs    map[string]string
	children []*node
	text     string
	isText   bool
}

func (n *node) appendChild(child *node) { n.children = append(n.children, child) }

// builder mirrors the semantics of Python's html.parser.HTMLParser subclass.
type builder struct {
	root  *node
	stack []*node
}

func newBuilder() *builder {
	root := &node{tag: "document"}
	return &builder{root: root, stack: []*node{root}}
}

func (b *builder) top() *node { return b.stack[len(b.stack)-1] }

// closeOpen unwinds to the nearest open tag in tags, stopping at boundaries.
func (b *builder) closeOpen(tags, boundaries map[string]bool) {
	for index := len(b.stack) - 1; index > 0; index-- {
		if boundaries[b.stack[index].tag] {
			return
		}
		if tags[b.stack[index].tag] {
			b.stack = b.stack[:index]
			return
		}
	}
}

var (
	liTags         = map[string]bool{"li": true}
	listBoundaries = map[string]bool{"ol": true, "ul": true}
	pTags          = map[string]bool{"p": true}
	trTags         = map[string]bool{"tr": true}
	tableSections  = map[string]bool{"table": true, "tbody": true, "tfoot": true, "thead": true}
	cellTags       = map[string]bool{"td": true, "th": true}
	rowBoundary    = map[string]bool{"tr": true}
	blockNotP      = blockTagsExcept("p")
)

func blockTagsExcept(name string) map[string]bool {
	result := make(map[string]bool, len(blockTags))
	for tag := range blockTags {
		if tag != name {
			result[tag] = true
		}
	}
	return result
}

func (b *builder) startTag(name string, attrs []html.Attribute) *node {
	switch name {
	case "li":
		b.closeOpen(liTags, listBoundaries)
	case "p":
		b.closeOpen(pTags, blockNotP)
	case "tr":
		b.closeOpen(trTags, tableSections)
	case "td", "th":
		b.closeOpen(cellTags, rowBoundary)
	}
	element := &node{tag: name, attrs: make(map[string]string, len(attrs))}
	for _, attr := range attrs {
		key := strings.ToLower(attr.Key)
		// Python's dict comprehension keeps the last occurrence of a duplicate.
		element.attrs[key] = attr.Val
	}
	b.top().appendChild(element)
	if !voidTags[name] {
		b.stack = append(b.stack, element)
	}
	return element
}

func (b *builder) endTag(name string) {
	for index := len(b.stack) - 1; index > 0; index-- {
		if b.stack[index].tag == name {
			b.stack = b.stack[:index]
			return
		}
	}
}

func (b *builder) data(text string) {
	if text != "" {
		b.top().appendChild(&node{isText: true, text: text})
	}
}

func parse(document string) *node {
	b := newBuilder()
	tokenizer := html.NewTokenizer(strings.NewReader(document))
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			return b.root
		case html.TextToken:
			b.data(string(tokenizer.Text()))
		case html.StartTagToken:
			token := tokenizer.Token()
			b.startTag(strings.ToLower(token.Data), token.Attr)
		case html.SelfClosingTagToken:
			token := tokenizer.Token()
			name := strings.ToLower(token.Data)
			b.startTag(name, token.Attr)
			if b.top().tag == name {
				b.stack = b.stack[:len(b.stack)-1]
			}
		case html.EndTagToken:
			token := tokenizer.Token()
			b.endTag(strings.ToLower(token.Data))
		case html.CommentToken, html.DoctypeToken:
			// Dropped, matching HTMLParser's default no-op handlers.
		}
	}
}

// text returns the concatenated character data beneath a node.
func text(n *node) string {
	if n.isText {
		return n.text
	}
	var out strings.Builder
	for _, child := range n.children {
		out.WriteString(text(child))
	}
	return out.String()
}

// fallbackText extracts safe text iteratively when rendering is defeated by
// pathological nesting.
func fallbackText(root *node) string {
	var parts []string
	pending := []*node{root}
	for len(pending) > 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if current.isText {
			parts = append(parts, current.text)
			continue
		}
		if ignoredTags[current.tag] {
			continue
		}
		for index := len(current.children) - 1; index >= 0; index-- {
			pending = append(pending, current.children[index])
		}
		if blockTags[current.tag] || current.tag == "br" || current.tag == "li" {
			parts = append(parts, "\n")
		}
	}
	return textx.CollapseStrip(strings.Join(parts, ""))
}

func label(value, fallback string) string {
	value = textx.CollapseStrip(value)
	if value == "" {
		value = fallback
	}
	value = strings.ReplaceAll(value, "[", `\[`)
	return strings.ReplaceAll(value, "]", `\]`)
}

func inlineCode(value string) string {
	value = textx.CollapseStrip(value)
	if value == "" {
		return ""
	}
	fence := "`"
	if strings.Contains(value, "`") {
		fence = "``"
	}
	padding := ""
	if strings.HasPrefix(value, "`") || strings.HasSuffix(value, "`") {
		padding = " "
	}
	return fence + padding + value + padding + fence
}

func renderTable(n *node, baseURL string) string {
	type row struct {
		cells  []string
		header bool
	}
	var rows []row

	var visit func(current *node)
	visit = func(current *node) {
		if current.tag == "tr" {
			var cells []string
			header := false
			for _, child := range current.children {
				if child.isText || !cellTags[child.tag] {
					continue
				}
				cell := textx.Collapse(textx.Strip(render(child, baseURL)))
				cells = append(cells, cell)
				if child.tag == "th" {
					header = true
				}
			}
			if len(cells) > 0 {
				rows = append(rows, row{cells: cells, header: header})
			}
			return
		}
		for _, child := range current.children {
			if !child.isText {
				visit(child)
			}
		}
	}
	visit(n)
	if len(rows) == 0 {
		return ""
	}

	width := 0
	for _, item := range rows {
		if len(item.cells) > width {
			width = len(item.cells)
		}
	}
	for index := range rows {
		for len(rows[index].cells) < width {
			rows[index].cells = append(rows[index].cells, "")
		}
	}
	headerIndex := 0
	for index, item := range rows {
		if item.header {
			headerIndex = index
			break
		}
	}
	header := rows[headerIndex].cells
	rest := append(append([]row{}, rows[:headerIndex]...), rows[headerIndex+1:]...)

	renderRow := func(cells []string) string {
		escaped := make([]string, len(cells))
		for index, cell := range cells {
			escaped[index] = strings.ReplaceAll(cell, "|", `\|`)
		}
		return "| " + strings.Join(escaped, " | ") + " |"
	}

	separator := make([]string, width)
	for index := range separator {
		separator[index] = "---"
	}
	lines := []string{renderRow(header), renderRow(separator)}
	for _, item := range rest {
		lines = append(lines, renderRow(item.cells))
	}
	return strings.Join(lines, "\n")
}

func renderList(n *node, baseURL string, depth int) string {
	var lines []string
	number := 1
	for _, child := range n.children {
		if child.isText || child.tag != "li" {
			continue
		}
		var nested []*node
		var body strings.Builder
		for _, item := range child.children {
			if !item.isText && (item.tag == "ul" || item.tag == "ol") {
				nested = append(nested, item)
				continue
			}
			if item.isText {
				body.WriteString(item.text)
			} else {
				body.WriteString(render(item, baseURL))
			}
		}
		flattened := textx.Strip(spaceAroundNL.ReplaceAllString(body.String(), " "))
		marker := "-"
		if n.tag == "ol" {
			marker = itoa(number) + "."
		}
		line := strings.TrimRight(strings.Repeat("  ", depth)+marker+" "+flattened, " \t")
		lines = append(lines, line)
		for _, nestedList := range nested {
			lines = append(lines, renderList(nestedList, baseURL, depth+1))
		}
		number++
	}
	kept := lines[:0]
	for _, line := range lines {
		if line != "" {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

func render(n *node, baseURL string) string {
	if n.isText {
		return textx.Collapse(n.text)
	}
	tag := n.tag
	if ignoredTags[tag] {
		return ""
	}
	if tag == "pre" {
		value := strings.Trim(text(n), "\n")
		if value == "" {
			return ""
		}
		fence := "```"
		if strings.Contains(value, "```") {
			fence = "````"
		}
		language := ""
		for _, child := range n.children {
			if child.isText {
				continue
			}
			if child.tag == "code" {
				for _, class := range strings.Fields(child.attrs["class"]) {
					if after, found := strings.CutPrefix(class, "language-"); found {
						language = after
						break
					}
				}
			}
			break
		}
		return "\n\n" + fence + language + "\n" + value + "\n" + fence + "\n\n"
	}

	var builder strings.Builder
	for _, child := range n.children {
		if child.isText {
			builder.WriteString(textx.Collapse(child.text))
		} else {
			builder.WriteString(render(child, baseURL))
		}
	}
	children := builder.String()
	trimmed := textx.Strip(children)

	switch {
	case tag == "document":
		return children
	case blockTags[tag]:
		if trimmed == "" {
			return ""
		}
		return "\n\n" + trimmed + "\n\n"
	case len(tag) == 2 && tag[0] == 'h' && tag[1] >= '1' && tag[1] <= '6':
		return "\n\n" + strings.Repeat("#", int(tag[1]-'0')) + " " + trimmed + "\n\n"
	case tag == "strong" || tag == "b":
		if trimmed == "" {
			return ""
		}
		return "**" + trimmed + "**"
	case tag == "em" || tag == "i":
		if trimmed == "" {
			return ""
		}
		return "*" + trimmed + "*"
	case tag == "del" || tag == "s" || tag == "strike":
		if trimmed == "" {
			return ""
		}
		return "~~" + trimmed + "~~"
	case tag == "mark":
		if trimmed == "" {
			return ""
		}
		return "==" + trimmed + "=="
	case tag == "code":
		return inlineCode(text(n))
	case tag == "br":
		return "\n"
	case tag == "hr":
		return "\n\n---\n\n"
	case tag == "a":
		href := safeURL(n.attrs["href"], baseURL)
		fallback := href
		if fallback == "" {
			fallback = "link"
		}
		text := label(children, fallback)
		if href == "" {
			return text
		}
		return "[" + text + "](<" + href + ">)"
	case tag == "img":
		source := safeURL(n.attrs["src"], baseURL)
		if source == "" {
			return ""
		}
		return "![" + label(n.attrs["alt"], "image") + "](<" + source + ">)"
	case tag == "ul" || tag == "ol":
		return "\n\n" + renderList(n, baseURL, 0) + "\n\n"
	case tag == "li":
		return children
	case tag == "blockquote":
		lines := textx.SplitLines(trimmed)
		quoted := make([]string, len(lines))
		for index, line := range lines {
			if line == "" {
				quoted[index] = ">"
			} else {
				quoted[index] = "> " + line
			}
		}
		// An empty blockquote yields no lines at all, not a bare ">".
		return "\n\n" + strings.Join(quoted, "\n") + "\n\n"
	case tag == "table":
		table := renderTable(n, baseURL)
		if table == "" {
			return ""
		}
		return "\n\n" + table + "\n\n"
	}
	return children
}

// ToMarkdown converts feed HTML to conservative Obsidian-compatible Markdown.
//
// Plain text and existing Markdown pass through unchanged. Unsupported tags
// lose only their markup, while executable and styling elements are removed
// with their contents.
func ToMarkdown(value string, baseURL string) string {
	if value == "" {
		return ""
	}
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	if !htmlTagPattern.MatchString(value) {
		return textx.Strip(value)
	}
	root := parse(value)
	result := render(root, baseURL)
	result = trailingSpace.ReplaceAllString(result, "\n")
	result = blankRun.ReplaceAllString(result, "\n\n")
	return textx.Strip(result)
}

// FallbackText is exported for the ingestion path, which needs a plain-text
// projection of the same tree.
func FallbackText(value string) string {
	return fallbackText(parse(value))
}
