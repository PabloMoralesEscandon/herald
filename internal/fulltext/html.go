package fulltext

import (
	"strings"

	"github.com/PabloMoralesEscandon/herald/internal/textx"
	"github.com/PabloMoralesEscandon/herald/internal/urlx"
	xhtml "golang.org/x/net/html"
)

// The HTML path is the preferred one wherever it exists.
//
// arXiv now publishes a LaTeXML rendering of most papers, and that rendering
// states outright what a PDF only implies: which text is a section heading,
// which span is a citation, and exactly which bibliography entry each citation
// points at. Reading it turns citation linking from a heuristic into a lookup.
//
// Unlike internal/markdown, which parses feed HTML with a deliberately small
// token-level parser to keep note output stable, this uses the conforming
// parser. Feed content is a short fragment Herald controls the rules for; a
// published article page is a full document with real-world markup errors, and
// the conforming parser's error recovery is what makes it readable at all.

// FromHTML extracts an article from an HTML page.
func FromHTML(page []byte, baseURL string) (*Document, error) {
	root, err := xhtml.Parse(strings.NewReader(string(page)))
	if err != nil {
		return nil, &Error{Reason: "Herald could not parse the article page"}
	}
	document := &Document{Format: FormatHTML}
	walker := &htmlWalker{
		document: document,
		base:     baseURL,
		anchors:  map[string]string{},
	}

	walker.collectBibliography(root)
	document.References = walker.references
	walker.walkBlocks(walker.contentRoot(root), 0)

	if document.Title == "" {
		document.Title = walker.title
	}
	// Only the HTML pages that mark their citations up give perfect links.
	// A page that does not is still worth scanning with the text heuristics.
	if !walker.linkedAnyCitation && len(document.References) > 0 {
		index := NewIndex(document.References)
		for position := range document.Blocks {
			document.Blocks[position].Text = index.Link(document.Blocks[position].Text)
		}
	}
	return document, nil
}

// Error is a user-facing extraction failure.
type Error struct{ Reason string }

func (e *Error) Error() string { return e.Reason }

type htmlWalker struct {
	document *Document
	base     string
	title    string

	// anchors maps a bibliography item's element id to its reference key, so a
	// citation anchor resolves without any text matching at all.
	anchors           map[string]string
	references        []Reference
	linkedAnyCitation bool
}

const maxHTMLDepth = 100

// skippedTags never contribute text.
var skippedTags = map[string]bool{
	"script": true, "style": true, "noscript": true, "svg": true,
	"template": true, "head": true, "form": true, "button": true,
	"iframe": true, "object": true, "embed": true, "select": true,
}

// skippedClasses are the page furniture LaTeXML and publisher pages wrap
// around an article.
var skippedClasses = []string{
	"ltx_page_navbar", "ltx_page_footer", "ltx_pagination", "ltx_authors",
	"ltx_dates", "ltx_role_institute", "ltx_bibliography", "ltx_biblist",
	"ref-list", "references-list", "navbar", "sidebar", "cookie",
	"skip-link", "site-header", "site-footer",
}

func attribute(node *xhtml.Node, name string) string {
	for _, item := range node.Attr {
		if strings.EqualFold(item.Key, name) {
			return item.Val
		}
	}
	return ""
}

func hasClass(node *xhtml.Node, class string) bool {
	for _, field := range strings.Fields(attribute(node, "class")) {
		if field == class {
			return true
		}
	}
	return false
}

func classContainsAny(node *xhtml.Node, needles []string) bool {
	classes := attribute(node, "class") + " " + attribute(node, "id")
	if classes == " " {
		return false
	}
	lowered := strings.ToLower(classes)
	for _, needle := range needles {
		if strings.Contains(lowered, needle) {
			return true
		}
	}
	return false
}

// contentRoot picks the element holding the article, preferring the containers
// publishers mark explicitly over the whole body.
func (w *htmlWalker) contentRoot(root *xhtml.Node) *xhtml.Node {
	var body, best *xhtml.Node
	var visit func(node *xhtml.Node, depth int)
	visit = func(node *xhtml.Node, depth int) {
		if node == nil || depth > maxHTMLDepth || best != nil {
			return
		}
		if node.Type == xhtml.ElementNode {
			switch {
			case node.Data == "body":
				body = node
			case hasClass(node, "ltx_page_content"), hasClass(node, "ltx_document"):
				best = node
				return
			case node.Data == "article":
				best = node
				return
			}
			if w.title == "" && node.Data == "title" {
				w.title = textx.CollapseStrip(textContent(node, 0))
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child, depth+1)
		}
	}
	visit(root, 0)
	switch {
	case best != nil:
		return best
	case body != nil:
		return body
	default:
		return root
	}
}

// collectBibliography records every bibliography entry and the element id a
// citation would use to reach it.
func (w *htmlWalker) collectBibliography(root *xhtml.Node) {
	var visit func(node *xhtml.Node, depth int)
	visit = func(node *xhtml.Node, depth int) {
		if node == nil || depth > maxHTMLDepth || len(w.references) >= MaxReferences {
			return
		}
		if node.Type == xhtml.ElementNode && isBibliographyItem(node) {
			w.addBibliographyItem(node)
			return
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child, depth+1)
		}
	}
	visit(root, 0)
}

// isBibliographyItem recognizes the markup used for one reference entry.
func isBibliographyItem(node *xhtml.Node) bool {
	if hasClass(node, "ltx_bibitem") {
		return true
	}
	if node.Data != "li" {
		return false
	}
	// A list item directly inside a reference list is an entry.
	for parent := node.Parent; parent != nil; parent = parent.Parent {
		if parent.Type != xhtml.ElementNode {
			continue
		}
		if hasClass(parent, "ltx_biblist") || classContainsAny(parent, []string{"ref-list", "references"}) {
			return true
		}
		if parent.Data == "ol" || parent.Data == "ul" {
			continue
		}
		break
	}
	return false
}

func (w *htmlWalker) addBibliographyItem(node *xhtml.Node) {
	raw := textx.CollapseStrip(textContent(node, 0))
	if raw == "" {
		return
	}
	label := ""
	// LaTeXML prints the printed label, such as "[12]", in its own span.
	var findTag func(*xhtml.Node, int)
	findTag = func(current *xhtml.Node, depth int) {
		if current == nil || depth > 12 || label != "" {
			return
		}
		if current.Type == xhtml.ElementNode && hasClass(current, "ltx_bibtag") {
			label = textx.CollapseStrip(textContent(current, 0))
			return
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			findTag(child, depth+1)
		}
	}
	findTag(node, 0)
	if label != "" {
		raw = strings.TrimSpace(strings.TrimPrefix(raw, label))
	}

	reference := ParseReference(label, raw, len(w.references)+1)
	// Identifiers are often only in the entry's links, not its printed text.
	w.applyLinkedIdentifiers(node, &reference)
	w.references = append(w.references, reference)

	if id := attribute(node, "id"); id != "" {
		w.anchors[id] = reference.Key
	}
}

// applyLinkedIdentifiers reads DOI and arXiv identifiers out of an entry's
// hyperlinks, which is where structured renderings put them.
func (w *htmlWalker) applyLinkedIdentifiers(node *xhtml.Node, reference *Reference) {
	var visit func(*xhtml.Node, int)
	visit = func(current *xhtml.Node, depth int) {
		if current == nil || depth > 12 {
			return
		}
		if current.Type == xhtml.ElementNode && current.Data == "a" {
			href := attribute(current, "href")
			if reference.DOI == "" {
				if match := doiInText.FindString(href); match != "" {
					reference.DOI = strings.ToLower(strings.TrimRight(match, ".,;)"))
				}
			}
			if reference.ArxivID == "" {
				if match := arxivURLInText.FindStringSubmatch(href); match != nil {
					reference.ArxivID = normalizeArxiv(match[1])
				}
			}
			if reference.URL == "" && strings.HasPrefix(href, "http") {
				reference.URL = href
			}
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			visit(child, depth+1)
		}
	}
	visit(node, 0)
	reference.Key = referenceKey(*reference)
}

// walkBlocks converts block-level structure into document blocks.
func (w *htmlWalker) walkBlocks(node *xhtml.Node, depth int) {
	if node == nil || depth > maxHTMLDepth || len(w.document.Blocks) >= MaxBlocks {
		return
	}
	if node.Type == xhtml.ElementNode {
		if skippedTags[node.Data] || classContainsAny(node, skippedClasses) {
			return
		}
		switch node.Data {
		case "h1", "h2", "h3", "h4", "h5", "h6":
			level := int(node.Data[1] - '0')
			text := w.inline(node, 0)
			if isReferencesHeading(text) {
				// Herald renders the reference list itself, from every edge it
				// knows, so the article's own list is not repeated here.
				return
			}
			w.document.appendBlock(BlockHeading, level, text)
			return
		case "p":
			w.document.appendBlock(BlockParagraph, 0, w.inline(node, 0))
			return
		case "li":
			w.document.appendBlock(BlockListItem, 0, w.inline(node, 0))
			return
		case "blockquote":
			w.document.appendBlock(BlockQuote, 0, w.inline(node, 0))
			return
		case "pre":
			w.document.appendBlock(BlockCode, 0, textContent(node, 0))
			return
		case "figcaption", "caption":
			w.document.appendBlock(BlockCaption, 0, w.inline(node, 0))
			return
		case "table":
			w.document.appendBlock(BlockTable, 0, w.table(node))
			return
		case "math":
			if attribute(node, "display") == "block" {
				if latex := attribute(node, "alttext"); latex != "" {
					w.document.appendBlock(BlockMath, 0, latex)
					return
				}
			}
		}
		if hasClass(node, "ltx_abstract") {
			w.document.Abstract = textx.CollapseStrip(textContent(node, 0))
			return
		}
		if hasClass(node, "ltx_title_document") && w.document.Title == "" {
			w.document.Title = w.inline(node, 0)
			return
		}
	}

	// A container with no block-level children holds loose text that would
	// otherwise be dropped.
	if node.Type == xhtml.ElementNode && !hasBlockChild(node, 0) {
		if text := w.inline(node, 0); text != "" {
			w.document.appendBlock(BlockParagraph, 0, text)
		}
		return
	}
	if node.Type == xhtml.TextNode {
		if text := textx.CollapseStrip(node.Data); text != "" {
			w.document.appendBlock(BlockParagraph, 0, text)
		}
		return
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		w.walkBlocks(child, depth+1)
	}
}

func isReferencesHeading(text string) bool {
	normalized := strings.ToLower(strings.Trim(textx.CollapseStrip(text), " .:0123456789"))
	switch normalized {
	case "references", "bibliography", "works cited", "literature cited", "reference":
		return true
	}
	return false
}

var blockLevelTags = map[string]bool{
	"p": true, "div": true, "section": true, "article": true, "ul": true,
	"ol": true, "li": true, "table": true, "blockquote": true, "pre": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"figure": true, "figcaption": true, "header": true, "footer": true,
	"main": true, "aside": true, "nav": true, "dl": true, "dd": true, "dt": true,
}

func hasBlockChild(node *xhtml.Node, depth int) bool {
	if depth > 4 {
		return false
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if child.Type != xhtml.ElementNode {
			continue
		}
		if blockLevelTags[child.Data] {
			return true
		}
		if hasBlockChild(child, depth+1) {
			return true
		}
	}
	return false
}

// inline renders an element's contents as Markdown inline text.
func (w *htmlWalker) inline(node *xhtml.Node, depth int) string {
	var builder strings.Builder
	w.renderInline(node, depth, &builder)
	return textx.CollapseStrip(builder.String())
}

func (w *htmlWalker) renderInline(node *xhtml.Node, depth int, out *strings.Builder) {
	if node == nil || depth > maxHTMLDepth || out.Len() > MaxBlockRunes*4 {
		return
	}
	if node.Type == xhtml.TextNode {
		out.WriteString(node.Data)
		return
	}
	if node.Type != xhtml.ElementNode {
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			w.renderInline(child, depth+1, out)
		}
		return
	}
	if skippedTags[node.Data] || classContainsAny(node, skippedClasses) {
		return
	}

	switch node.Data {
	case "br":
		out.WriteByte(' ')
		return
	case "math":
		// LaTeXML keeps the original LaTeX in alttext, which Obsidian renders
		// directly. It is far better than the flattened MathML text.
		if latex := attribute(node, "alttext"); latex != "" {
			out.WriteString("$" + strings.TrimSpace(latex) + "$")
			return
		}
	case "cite":
		if w.renderCitation(node, out) {
			return
		}
	case "a":
		if w.renderAnchor(node, depth, out) {
			return
		}
	case "strong", "b":
		w.wrap(node, depth, "**", out)
		return
	case "em", "i":
		w.wrap(node, depth, "*", out)
		return
	case "code", "tt", "kbd":
		w.wrap(node, depth, "`", out)
		return
	case "sup":
		// A superscript that is only a citation marker is handled above; the
		// rest are exponents and footnote marks, kept as written.
		w.wrap(node, depth, "^", out)
		return
	}

	for child := node.FirstChild; child != nil; child = child.NextSibling {
		w.renderInline(child, depth+1, out)
	}
}

func (w *htmlWalker) wrap(node *xhtml.Node, depth int, marker string, out *strings.Builder) {
	var inner strings.Builder
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		w.renderInline(child, depth+1, &inner)
	}
	text := strings.TrimSpace(inner.String())
	if text == "" {
		return
	}
	// Emphasis around a citation placeholder would break the placeholder's
	// meaning once it becomes a link, so it is dropped.
	if strings.Contains(text, "{{herald:cite:") {
		out.WriteString(text)
		return
	}
	out.WriteString(marker + text + marker)
}

// renderCitation turns a citation element into placeholders, following its
// anchors to the exact bibliography entries it names.
func (w *htmlWalker) renderCitation(node *xhtml.Node, out *strings.Builder) bool {
	type target struct {
		key  string
		text string
	}
	var targets []target
	var visit func(*xhtml.Node, int)
	visit = func(current *xhtml.Node, depth int) {
		if current == nil || depth > 12 {
			return
		}
		if current.Type == xhtml.ElementNode && current.Data == "a" {
			href := attribute(current, "href")
			if key, ok := w.anchors[strings.TrimPrefix(href, "#")]; ok {
				targets = append(targets, target{
					key: key, text: textx.CollapseStrip(textContent(current, 0)),
				})
			}
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			visit(child, depth+1)
		}
	}
	visit(node, 0)
	if len(targets) == 0 {
		return false
	}

	// The surrounding punctuation the article printed is kept, so the note
	// reads exactly as the article does.
	full := textx.CollapseStrip(textContent(node, 0))
	rendered := full
	for _, item := range targets {
		if item.text == "" {
			continue
		}
		rendered = strings.Replace(rendered, item.text, CitePlaceholder(item.key, item.text), 1)
	}
	if rendered == full && len(targets) == 1 {
		// The brackets a citation is printed in stay outside the placeholder:
		// they are punctuation the article wrote, not part of the link, and an
		// Obsidian link alias cannot contain them.
		inner := strings.Trim(full, "[]()")
		prefix := full[:len(full)-len(strings.TrimLeft(full, "[("))]
		suffix := full[len(strings.TrimRight(full, "])")):]
		rendered = prefix + CitePlaceholder(targets[0].key, inner) + suffix
	}
	out.WriteString(rendered)
	w.linkedAnyCitation = true
	return true
}

// renderAnchor writes a Markdown link, resolving relative targets against the
// page it came from.
func (w *htmlWalker) renderAnchor(node *xhtml.Node, depth int, out *strings.Builder) bool {
	href := strings.TrimSpace(attribute(node, "href"))
	var inner strings.Builder
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		w.renderInline(child, depth+1, &inner)
	}
	text := textx.CollapseStrip(inner.String())
	if text == "" {
		return true
	}

	// An anchor into the article's own bibliography is a citation even when it
	// is not wrapped in a <cite> element.
	if key, ok := w.anchors[strings.TrimPrefix(href, "#")]; ok {
		out.WriteString(CitePlaceholder(key, text))
		w.linkedAnyCitation = true
		return true
	}
	if href == "" || strings.HasPrefix(href, "#") || strings.HasPrefix(href, "javascript:") {
		out.WriteString(text)
		return true
	}
	resolved := href
	if w.base != "" {
		resolved = urlx.Join(w.base, href)
	}
	if scheme := urlx.Scheme(resolved); scheme != "http" && scheme != "https" {
		out.WriteString(text)
		return true
	}
	out.WriteString("[" + escapeLinkText(text) + "](" + resolved + ")")
	return true
}

func escapeLinkText(text string) string {
	return strings.NewReplacer("[", "(", "]", ")").Replace(text)
}

// table renders an HTML table as a Markdown table.
func (w *htmlWalker) table(node *xhtml.Node) string {
	var rows [][]string
	var visit func(*xhtml.Node, int)
	visit = func(current *xhtml.Node, depth int) {
		if current == nil || depth > 20 || len(rows) > 400 {
			return
		}
		if current.Type == xhtml.ElementNode && current.Data == "tr" {
			var cells []string
			for cell := current.FirstChild; cell != nil; cell = cell.NextSibling {
				if cell.Type == xhtml.ElementNode && (cell.Data == "td" || cell.Data == "th") {
					text := w.inline(cell, 0)
					cells = append(cells, strings.ReplaceAll(text, "|", "\\|"))
				}
			}
			if len(cells) > 0 {
				rows = append(rows, cells)
			}
			return
		}
		for child := current.FirstChild; child != nil; child = child.NextSibling {
			visit(child, depth+1)
		}
	}
	visit(node, 0)
	if len(rows) == 0 {
		return ""
	}

	width := 0
	for _, row := range rows {
		width = max(width, len(row))
	}
	var lines []string
	for index, row := range rows {
		for len(row) < width {
			row = append(row, "")
		}
		lines = append(lines, "| "+strings.Join(row, " | ")+" |")
		if index == 0 {
			lines = append(lines, "|"+strings.Repeat(" --- |", width))
		}
	}
	return strings.Join(lines, "\n")
}

// textContent flattens an element to plain text.
func textContent(node *xhtml.Node, depth int) string {
	if node == nil || depth > maxHTMLDepth {
		return ""
	}
	if node.Type == xhtml.TextNode {
		return node.Data
	}
	if node.Type == xhtml.ElementNode && skippedTags[node.Data] {
		return ""
	}
	var builder strings.Builder
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		builder.WriteString(textContent(child, depth+1))
		if builder.Len() > MaxBlockRunes*4 {
			break
		}
	}
	return builder.String()
}
