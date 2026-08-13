package fulltext

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/PabloMoralesEscandon/herald/internal/textx"
)

// DefaultGROBIDURL is the standard address of a locally running GROBID
// service. It is configurable by callers so deployments can keep the service
// in another container or on another host.
const DefaultGROBIDURL = "http://127.0.0.1:8070"

const (
	defaultGROBIDTimeout = 2 * time.Minute
	maxGROBIDResponse    = 64 << 20
	maxTEIDepth          = 256
)

// GROBIDProcessor turns a PDF into GROBID's TEI XML response. Keeping this
// boundary small makes extraction testable without running the Java service.
type GROBIDProcessor interface {
	ProcessFulltext(document []byte) ([]byte, error)
}

// GROBIDProcessorFunc adapts a function into a GROBIDProcessor.
type GROBIDProcessorFunc func(document []byte) ([]byte, error)

func (f GROBIDProcessorFunc) ProcessFulltext(document []byte) ([]byte, error) {
	return f(document)
}

// GROBIDServiceError reports a configuration or availability failure in the
// extraction service, rather than a problem with one particular PDF.
type GROBIDServiceError struct{ Reason string }

func (e *GROBIDServiceError) Error() string { return e.Reason }

// GROBIDClient calls the processFulltextDocument REST endpoint.
type GROBIDClient struct {
	BaseURL    string
	HTTPClient *http.Client
	Timeout    time.Duration
}

// NewGROBIDClient builds a client for a GROBID service.
func NewGROBIDClient(baseURL string) *GROBIDClient {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DefaultGROBIDURL
	}
	return &GROBIDClient{BaseURL: strings.TrimRight(baseURL, "/")}
}

// ProcessFulltext submits a PDF as multipart/form-data and returns TEI XML.
func (c *GROBIDClient) ProcessFulltext(document []byte) ([]byte, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if baseURL == "" {
		baseURL = DefaultGROBIDURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, &GROBIDServiceError{Reason: "Herald's GROBID URL is invalid"}
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/api/processFulltextDocument"
	parsed.RawQuery = ""
	parsed.Fragment = ""

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = defaultGROBIDTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("input", "article.pdf")
	if err == nil {
		_, err = part.Write(document)
	}
	if err == nil {
		err = form.WriteField("includeRawCitations", "1")
	}
	if err == nil {
		// Consolidation reaches external metadata services and is unnecessary:
		// Herald merges the identifiers GROBID extracted itself.
		err = form.WriteField("consolidateHeader", "0")
	}
	if err == nil {
		err = form.WriteField("consolidateCitations", "0")
	}
	if closeErr := form.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, &Error{Reason: "Herald could not prepare the PDF for GROBID"}
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, parsed.String(), &body)
	if err != nil {
		return nil, &Error{Reason: "Herald could not prepare the GROBID request"}
	}
	request.Header.Set("Content-Type", form.FormDataContentType())
	request.Header.Set("Accept", "application/xml")

	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &GROBIDServiceError{
				Reason: "GROBID did not finish extracting the PDF before the timeout",
			}
		}
		return nil, &GROBIDServiceError{Reason: "Herald could not reach GROBID: " + err.Error()}
	}
	defer response.Body.Close()

	limited := io.LimitReader(response.Body, maxGROBIDResponse+1)
	payload, readErr := io.ReadAll(limited)
	if readErr != nil {
		return nil, &GROBIDServiceError{Reason: "Herald could not read GROBID's response"}
	}
	if len(payload) > maxGROBIDResponse {
		return nil, &Error{Reason: "GROBID returned more extracted text than Herald can safely keep"}
	}

	switch response.StatusCode {
	case http.StatusOK:
		return payload, nil
	case http.StatusNoContent:
		return nil, &ScannedError{Reason: "GROBID found no extractable text in the PDF"}
	case http.StatusServiceUnavailable:
		return nil, &GROBIDServiceError{
			Reason: "GROBID is busy or not ready; try the extraction again shortly",
		}
	default:
		detail := textx.CollapseStrip(string(payload))
		if runes := []rune(detail); len(runes) > 300 {
			detail = string(runes[:300])
		}
		reason := fmt.Sprintf("GROBID rejected the PDF with HTTP %d", response.StatusCode)
		if detail != "" {
			reason += ": " + detail
		}
		if response.StatusCode >= 500 {
			return nil, &GROBIDServiceError{Reason: reason}
		}
		return nil, &Error{Reason: reason}
	}
}

// FromPDF sends a PDF to GROBID and converts the returned TEI into Herald's
// article representation. Passing a processor is useful for tests; production
// callers normally use an Extractor configured with a GROBIDClient.
func FromPDF(data []byte, processor GROBIDProcessor) (*Document, error) {
	if !IsPDF(data) {
		return nil, &Error{Reason: "That file is not a PDF"}
	}
	if processor == nil {
		processor = NewGROBIDClient(DefaultGROBIDURL)
	}
	tei, err := processor.ProcessFulltext(data)
	if err != nil {
		return nil, err
	}
	return FromGROBIDTEI(tei)
}

// ScannedError reports a PDF for which GROBID could not recover article text.
// It remains distinct because retrying the same scan will not help.
type ScannedError struct{ Reason string }

func (e *ScannedError) Error() string { return e.Reason }

// xmlNode is a deliberately small mixed-content XML tree. GROBID's TEI uses
// inline elements inside paragraphs, so ordinary field-only XML structs would
// lose the order between text and citations.
type xmlNode struct {
	name    xml.Name
	attrs   []xml.Attr
	content []xmlContent
}

type xmlContent struct {
	text string
	node *xmlNode
}

// FromGROBIDTEI converts the TEI returned by processFulltextDocument.
func FromGROBIDTEI(tei []byte) (*Document, error) {
	root, err := parseXMLTree(tei)
	if err != nil {
		return nil, &Error{Reason: "GROBID returned invalid TEI XML: " + err.Error()}
	}
	if root == nil || root.name.Local != "TEI" {
		return nil, &Error{Reason: "GROBID returned a document that is not TEI XML"}
	}

	document := &Document{Format: FormatPDF}
	if header := firstDescendant(root, "teiHeader"); header != nil {
		if titleStmt := firstDescendant(header, "titleStmt"); titleStmt != nil {
			title := firstDescendant(titleStmt, "title")
			document.Title = collapsedText(title)
		}
		if abstract := firstDescendant(header, "abstract"); abstract != nil {
			document.Abstract = collapsedText(abstract)
		}
	}

	references, targets := parseTEIReferences(root)
	document.References = references
	if body := firstDescendant(root, "body"); body != nil {
		appendTEIBody(document, body, 0, targets)
	}
	if len(document.Blocks) == 0 || document.TextLength() == 0 {
		return nil, &ScannedError{Reason: "GROBID found no extractable article text in the PDF"}
	}

	// GROBID's bibliography targets are resolved while rendering inline
	// content. Unlike layout-based guessing, those links identify the exact
	// biblStruct the marker points to.
	return document, nil
}

func parseXMLTree(document []byte) (*xmlNode, error) {
	decoder := xml.NewDecoder(bytes.NewReader(document))
	decoder.Strict = true
	var root *xmlNode
	var stack []*xmlNode
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch value := token.(type) {
		case xml.StartElement:
			if len(stack) >= maxTEIDepth {
				return nil, fmt.Errorf("XML nesting exceeds Herald's limit")
			}
			node := &xmlNode{name: value.Name, attrs: append([]xml.Attr(nil), value.Attr...)}
			if len(stack) == 0 {
				if root != nil {
					return nil, fmt.Errorf("XML has more than one root element")
				}
				root = node
			} else {
				parent := stack[len(stack)-1]
				parent.content = append(parent.content, xmlContent{node: node})
			}
			stack = append(stack, node)
		case xml.CharData:
			if len(stack) > 0 {
				parent := stack[len(stack)-1]
				parent.content = append(parent.content, xmlContent{text: string(value)})
			}
		case xml.EndElement:
			if len(stack) == 0 {
				return nil, fmt.Errorf("XML contains an unexpected closing element")
			}
			stack = stack[:len(stack)-1]
		}
	}
	if len(stack) != 0 {
		return nil, fmt.Errorf("XML ended inside an element")
	}
	return root, nil
}

func (n *xmlNode) attr(name string) string {
	if n == nil {
		return ""
	}
	for _, attribute := range n.attrs {
		if attribute.Name.Local == name {
			return strings.TrimSpace(attribute.Value)
		}
	}
	return ""
}

func directChildren(node *xmlNode, name string) []*xmlNode {
	var children []*xmlNode
	if node == nil {
		return children
	}
	for _, part := range node.content {
		if part.node != nil && (name == "" || part.node.name.Local == name) {
			children = append(children, part.node)
		}
	}
	return children
}

func descendants(node *xmlNode, name string) []*xmlNode {
	var matches []*xmlNode
	var visit func(*xmlNode)
	visit = func(current *xmlNode) {
		for _, child := range directChildren(current, "") {
			if child.name.Local == name {
				matches = append(matches, child)
			}
			visit(child)
		}
	}
	if node != nil {
		visit(node)
	}
	return matches
}

func firstDescendant(node *xmlNode, name string) *xmlNode {
	if node == nil {
		return nil
	}
	for _, child := range directChildren(node, "") {
		if child.name.Local == name {
			return child
		}
		if found := firstDescendant(child, name); found != nil {
			return found
		}
	}
	return nil
}

func rawText(node *xmlNode) string {
	if node == nil {
		return ""
	}
	var out strings.Builder
	for _, part := range node.content {
		if part.node != nil {
			out.WriteString(rawText(part.node))
		} else {
			out.WriteString(part.text)
		}
	}
	return out.String()
}

func collapsedText(node *xmlNode) string { return textx.CollapseStrip(rawText(node)) }

func parseTEIReferences(root *xmlNode) ([]Reference, map[string]Reference) {
	var references []Reference
	targets := map[string]Reference{}
	for _, bibliography := range descendants(root, "biblStruct") {
		if len(references) >= MaxReferences {
			break
		}
		position := len(references) + 1
		label := bibliography.attr("n")
		if labelNode := firstDescendant(bibliography, "label"); label == "" && labelNode != nil {
			label = collapsedText(labelNode)
		}
		if label == "" {
			label = fmt.Sprint(position)
		}
		raw := ""
		for _, note := range descendants(bibliography, "note") {
			if strings.EqualFold(note.attr("type"), "raw_reference") {
				raw = collapsedText(note)
				break
			}
		}
		if raw == "" {
			raw = collapsedText(bibliography)
		}
		reference := ParseReference(label, raw, position)

		if analytic := firstDescendant(bibliography, "analytic"); analytic != nil {
			for _, title := range descendants(analytic, "title") {
				if value := collapsedText(title); value != "" {
					reference.Title = value
					break
				}
			}
			if authors := teiAuthors(analytic); authors != "" {
				reference.Authors = authors
			}
		}
		for _, identifier := range descendants(bibliography, "idno") {
			value := strings.TrimSpace(collapsedText(identifier))
			switch strings.ToLower(identifier.attr("type")) {
			case "doi":
				value = strings.ToLower(value)
				for _, prefix := range []string{"https://doi.org/", "http://dx.doi.org/", "doi:"} {
					value = strings.TrimPrefix(value, prefix)
				}
				reference.DOI = strings.TrimRight(strings.TrimSpace(value), ".,;)")
			case "arxiv":
				reference.ArxivID = normalizeArxiv(strings.TrimPrefix(strings.ToLower(value), "arxiv:"))
			}
		}
		if date := firstDescendant(bibliography, "date"); date != nil {
			if value := date.attr("when"); value != "" {
				if match := yearInText.FindString(value); match != "" {
					reference.Year = match
				}
			}
		}
		for _, pointer := range append(descendants(bibliography, "ptr"), descendants(bibliography, "ref")...) {
			target := pointer.attr("target")
			if parsed, err := url.Parse(target); err == nil &&
				(parsed.Scheme == "http" || parsed.Scheme == "https") {
				reference.URL = target
				break
			}
		}
		reference.Key = referenceKey(reference)
		references = append(references, reference)
		if id := bibliography.attr("id"); id != "" {
			targets[strings.TrimPrefix(id, "#")] = reference
		}
	}
	return references, targets
}

func teiAuthors(node *xmlNode) string {
	var authors []string
	for _, author := range descendants(node, "author") {
		name := firstDescendant(author, "persName")
		if name == nil {
			continue
		}
		var parts []string
		for _, forename := range descendants(name, "forename") {
			if value := collapsedText(forename); value != "" {
				parts = append(parts, value)
			}
		}
		for _, surname := range descendants(name, "surname") {
			if value := collapsedText(surname); value != "" {
				parts = append(parts, value)
			}
		}
		if len(parts) == 0 {
			parts = append(parts, collapsedText(name))
		}
		if value := strings.TrimSpace(strings.Join(parts, " ")); value != "" {
			authors = append(authors, value)
		}
	}
	return strings.Join(authors, ", ")
}

func appendTEIBody(document *Document, node *xmlNode, level int, targets map[string]Reference) {
	for _, child := range directChildren(node, "") {
		switch child.name.Local {
		case "div":
			appendTEIBody(document, child, min(level+1, 6), targets)
		case "head":
			text := renderTEIInline(child, targets)
			if number := child.attr("n"); number != "" && !strings.HasPrefix(text, number) {
				text = number + " " + text
			}
			document.appendBlock(BlockHeading, level, text)
		case "p", "ab":
			document.appendBlock(BlockParagraph, 0, renderTEIInline(child, targets))
		case "list":
			for _, item := range directChildren(child, "item") {
				document.appendBlock(BlockListItem, 0, renderTEIInline(item, targets))
			}
		case "quote":
			document.appendBlock(BlockQuote, 0, renderTEIInline(child, targets))
		case "formula":
			document.appendBlock(BlockMath, 0, collapsedText(child))
		case "figure":
			appendTEIFigure(document, child, targets)
		case "code":
			document.appendBlock(BlockCode, 0, rawText(child))
		case "note":
			// Footnotes and marginal notes are not part of the body flow.
		default:
			appendTEIBody(document, child, level, targets)
		}
	}
}

func renderTEIInline(node *xmlNode, targets map[string]Reference) string {
	var out strings.Builder
	var render func(*xmlNode)
	render = func(current *xmlNode) {
		for _, part := range current.content {
			if part.node == nil {
				out.WriteString(part.text)
				continue
			}
			child := part.node
			switch child.name.Local {
			case "ref":
				display := collapsedText(child)
				if strings.EqualFold(child.attr("type"), "bibr") {
					out.WriteString(linkTEICitation(display, child.attr("target"), targets))
				} else if target := child.attr("target"); target != "" {
					if parsed, err := url.Parse(target); err == nil &&
						(parsed.Scheme == "http" || parsed.Scheme == "https") {
						out.WriteString("[" + display + "](" + target + ")")
					} else {
						out.WriteString(display)
					}
				} else {
					out.WriteString(display)
				}
			case "hi":
				value := collapsedText(child)
				rend := strings.ToLower(child.attr("rend"))
				if strings.Contains(rend, "bold") {
					out.WriteString("**" + value + "**")
				} else if strings.Contains(rend, "italic") {
					out.WriteString("*" + value + "*")
				} else {
					out.WriteString(value)
				}
			case "formula":
				out.WriteString("$" + collapsedText(child) + "$")
			case "lb":
				out.WriteByte('\n')
			default:
				render(child)
			}
		}
	}
	render(node)
	return textx.CollapseStrip(out.String())
}

// linkTEICitation maps a TEI marker to the exact bibliography targets GROBID
// supplied. A citation cluster can point at several biblStruct elements; in
// that case every printed label is linked while brackets and separators stay
// exactly as the paper rendered them.
func linkTEICitation(display, targetList string, targets map[string]Reference) string {
	var references []Reference
	for _, target := range strings.Fields(targetList) {
		if reference, ok := targets[strings.TrimPrefix(target, "#")]; ok {
			references = append(references, reference)
		} else {
			return display
		}
	}
	if len(references) == 0 {
		return display
	}
	if len(references) == 1 {
		return CitePlaceholder(references[0].Key, display)
	}

	remaining := display
	var out strings.Builder
	for _, reference := range references {
		label := strings.TrimSpace(reference.Label)
		if label == "" {
			return display
		}
		position := strings.Index(remaining, label)
		if position < 0 {
			return display
		}
		out.WriteString(remaining[:position])
		out.WriteString(CitePlaceholder(reference.Key, label))
		remaining = remaining[position+len(label):]
	}
	out.WriteString(remaining)
	return out.String()
}

func appendTEIFigure(document *Document, figure *xmlNode, targets map[string]Reference) {
	caption := ""
	if head := firstDescendant(figure, "head"); head != nil {
		caption = renderTEIInline(head, targets)
	}
	if description := firstDescendant(figure, "figDesc"); description != nil {
		text := renderTEIInline(description, targets)
		if caption == "" {
			caption = text
		} else if text != "" {
			caption += ": " + text
		}
	}
	if caption != "" {
		document.appendBlock(BlockCaption, 0, caption)
	}
	if table := firstDescendant(figure, "table"); table != nil {
		if markdown := renderTEITable(table, targets); markdown != "" {
			document.appendBlock(BlockTable, 0, markdown)
		}
	}
}

func renderTEITable(table *xmlNode, targets map[string]Reference) string {
	var rows [][]string
	for _, row := range descendants(table, "row") {
		var cells []string
		for _, cell := range directChildren(row, "cell") {
			value := strings.ReplaceAll(renderTEIInline(cell, targets), "|", `\|`)
			cells = append(cells, value)
		}
		if len(cells) > 0 {
			rows = append(rows, cells)
		}
	}
	if len(rows) == 0 {
		return ""
	}
	columns := 0
	for _, row := range rows {
		columns = max(columns, len(row))
	}
	var lines []string
	rowLine := func(row []string) string {
		padded := append([]string(nil), row...)
		for len(padded) < columns {
			padded = append(padded, "")
		}
		return "| " + strings.Join(padded, " | ") + " |"
	}
	lines = append(lines, rowLine(rows[0]))
	separator := make([]string, columns)
	for index := range separator {
		separator[index] = "---"
	}
	lines = append(lines, rowLine(separator))
	for _, row := range rows[1:] {
		lines = append(lines, rowLine(row))
	}
	return strings.Join(lines, "\n")
}
