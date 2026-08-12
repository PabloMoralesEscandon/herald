package pdf

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"strings"
	"testing"
)

// builder assembles a syntactically correct PDF with a real cross-reference
// table, so the tests exercise the same path a produced file takes rather than
// only the damaged-file reconstruction fallback.
type builder struct {
	objects []string
}

func (b *builder) add(body string) int {
	b.objects = append(b.objects, body)
	return len(b.objects)
}

// addStream appends a stream object with a correct /Length.
func (b *builder) addStream(dict, content string) int {
	body := fmt.Sprintf("<<%s/Length %d>>\nstream\n%s\nendstream", dict, len(content), content)
	return b.add(body)
}

func (b *builder) build(rootObject int) []byte {
	var out bytes.Buffer
	out.WriteString("%PDF-1.7\n")

	offsets := make([]int, len(b.objects)+1)
	for index, body := range b.objects {
		offsets[index+1] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", index+1, body)
	}

	xrefAt := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n", len(b.objects)+1)
	out.WriteString("0000000000 65535 f \n")
	for index := 1; index <= len(b.objects); index++ {
		fmt.Fprintf(&out, "%010d 00000 n \n", offsets[index])
	}
	fmt.Fprintf(&out, "trailer\n<</Size %d/Root %d 0 R>>\nstartxref\n%d\n%%%%EOF\n",
		len(b.objects)+1, rootObject, xrefAt)
	return out.Bytes()
}

// simplePDF wraps one content stream in a single-page document using a
// standard font.
func simplePDF(t *testing.T, content string, fontExtra string) []byte {
	t.Helper()
	var b builder
	catalog := b.add("<</Type/Catalog/Pages 2 0 R>>")
	b.add("<</Type/Pages/Kids[3 0 R]/Count 1>>")
	b.add("<</Type/Page/Parent 2 0 R/MediaBox[0 0 612 792]" +
		"/Resources<</Font<</F1 5 0 R>>>>/Contents 4 0 R>>")
	b.addStream("", content)
	b.add("<</Type/Font/Subtype/Type1/BaseFont/Helvetica/Encoding/WinAnsiEncoding" +
		fontExtra + ">>")
	return b.build(catalog)
}

func extractText(t *testing.T, document []byte) string {
	t.Helper()
	result, err := Extract(document, Options{})
	if err != nil {
		t.Fatalf("Extract returned an error: %v", err)
	}
	return result.Text()
}

func TestExtractsPositionedText(t *testing.T) {
	content := `BT /F1 12 Tf 72 700 Td (Hello world) Tj ET`
	text := extractText(t, simplePDF(t, content, ""))
	if !strings.Contains(text, "Hello world") {
		t.Fatalf("expected the drawn text, got %q", text)
	}
}

func TestInsertsSpacesFromGeometry(t *testing.T) {
	// Two words drawn at separate positions with no space character between
	// them. This is how most typeset PDFs place words, and an extractor that
	// only concatenates strings runs them together.
	content := `BT /F1 12 Tf 72 700 Td (Herald) Tj 60 0 Td (reads) Tj ET`
	text := extractText(t, simplePDF(t, content, ""))
	if !strings.Contains(text, "Herald reads") {
		t.Fatalf("expected a space to be inferred, got %q", text)
	}
}

func TestTJArrayKerningDoesNotSplitWords(t *testing.T) {
	// Small negative adjustments are kerning inside a word, not a space.
	content := `BT /F1 12 Tf 72 700 Td [(Wa) -20 (ter) -15 (fall)] TJ ET`
	text := extractText(t, simplePDF(t, content, ""))
	if !strings.Contains(text, "Waterfall") {
		t.Fatalf("kerning should not split the word, got %q", text)
	}
}

func TestTJArrayLargeAdjustmentBecomesSpace(t *testing.T) {
	content := `BT /F1 12 Tf 72 700 Td [(one) -400 (two)] TJ ET`
	text := extractText(t, simplePDF(t, content, ""))
	if !strings.Contains(text, "one two") {
		t.Fatalf("a wide adjustment should read as a space, got %q", text)
	}
}

func TestReadsTwoColumnsInOrder(t *testing.T) {
	// A full-width title over two columns: the recursive cut must read the
	// title, then all of the left column, then all of the right column.
	var content strings.Builder
	content.WriteString("BT /F1 16 Tf 72 740 Td (Full Width Title) Tj ET\n")
	leftLines := []string{"left one", "left two", "left three"}
	rightLines := []string{"right one", "right two", "right three"}
	for index, line := range leftLines {
		fmt.Fprintf(&content, "BT /F1 10 Tf 72 %d Td (%s) Tj ET\n", 700-index*14, line)
	}
	for index, line := range rightLines {
		fmt.Fprintf(&content, "BT /F1 10 Tf 340 %d Td (%s) Tj ET\n", 700-index*14, line)
	}

	text := extractText(t, simplePDF(t, content.String(), ""))
	order := []string{
		"Full Width Title", "left one", "left two", "left three",
		"right one", "right two", "right three",
	}
	position := -1
	for _, want := range order {
		at := strings.Index(text, want)
		if at < 0 {
			t.Fatalf("missing %q in extracted text:\n%s", want, text)
		}
		if at < position {
			t.Fatalf("%q appeared out of reading order in:\n%s", want, text)
		}
		position = at
	}
}

func TestFlateCompressedContentStream(t *testing.T) {
	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	writer.Write([]byte(`BT /F1 12 Tf 72 700 Td (Compressed body) Tj ET`))
	writer.Close()

	var b builder
	catalog := b.add("<</Type/Catalog/Pages 2 0 R>>")
	b.add("<</Type/Pages/Kids[3 0 R]/Count 1>>")
	b.add("<</Type/Page/Parent 2 0 R/MediaBox[0 0 612 792]" +
		"/Resources<</Font<</F1 5 0 R>>>>/Contents 4 0 R>>")
	b.addStream("/Filter/FlateDecode", compressed.String())
	b.add("<</Type/Font/Subtype/Type1/BaseFont/Helvetica/Encoding/WinAnsiEncoding>>")

	text := extractText(t, b.build(catalog))
	if !strings.Contains(text, "Compressed body") {
		t.Fatalf("expected the decompressed text, got %q", text)
	}
}

func TestDifferencesEncodingResolvesLigatures(t *testing.T) {
	// pdfTeX writes ligatures as custom codes with glyph names and often no
	// ToUnicode map at all. Byte 1 is /fi here.
	font := "/Encoding<</BaseEncoding/WinAnsiEncoding/Differences[1/fi 2/quoteright]>>"
	var b builder
	catalog := b.add("<</Type/Catalog/Pages 2 0 R>>")
	b.add("<</Type/Pages/Kids[3 0 R]/Count 1>>")
	b.add("<</Type/Page/Parent 2 0 R/MediaBox[0 0 612 792]" +
		"/Resources<</Font<</F1 5 0 R>>>>/Contents 4 0 R>>")
	b.addStream("", `BT /F1 12 Tf 72 700 Td (classi\001cation\002s) Tj ET`)
	b.add("<</Type/Font/Subtype/Type1/BaseFont/NimbusRomNo9L-Regu" + font + ">>")

	text := extractText(t, b.build(catalog))
	if !strings.Contains(text, "classiﬁcation’s") {
		t.Fatalf("expected glyph names to resolve, got %q", text)
	}
}

func TestToUnicodeOverridesEncoding(t *testing.T) {
	cmap := `/CIDInit /ProcSet findresource begin
12 dict begin
begincmap
1 begincodespacerange
<00> <ff>
endcodespacerange
2 beginbfchar
<41> <03B1>
<42> <03B2>
endbfchar
endcmap
end
end`
	var b builder
	catalog := b.add("<</Type/Catalog/Pages 2 0 R>>")
	b.add("<</Type/Pages/Kids[3 0 R]/Count 1>>")
	b.add("<</Type/Page/Parent 2 0 R/MediaBox[0 0 612 792]" +
		"/Resources<</Font<</F1 5 0 R>>>>/Contents 4 0 R>>")
	b.addStream("", `BT /F1 12 Tf 72 700 Td (AB) Tj ET`)
	b.add("<</Type/Font/Subtype/Type1/BaseFont/CMMI10/Encoding/WinAnsiEncoding/ToUnicode 6 0 R>>")
	b.addStream("", cmap)

	text := extractText(t, b.build(catalog))
	if !strings.Contains(text, "αβ") {
		t.Fatalf("expected the ToUnicode mapping to win, got %q", text)
	}
}

func TestBfRangeMapsConsecutiveCodes(t *testing.T) {
	cmap := `begincmap
1 begincodespacerange
<00> <ff>
endcodespacerange
1 beginbfrange
<41> <43> <0391>
endbfrange
endcmap`
	var b builder
	catalog := b.add("<</Type/Catalog/Pages 2 0 R>>")
	b.add("<</Type/Pages/Kids[3 0 R]/Count 1>>")
	b.add("<</Type/Page/Parent 2 0 R/MediaBox[0 0 612 792]" +
		"/Resources<</Font<</F1 5 0 R>>>>/Contents 4 0 R>>")
	b.addStream("", `BT /F1 12 Tf 72 700 Td (ABC) Tj ET`)
	b.add("<</Type/Font/Subtype/Type1/BaseFont/Test/ToUnicode 6 0 R>>")
	b.addStream("", cmap)

	text := extractText(t, b.build(catalog))
	if !strings.Contains(text, "ΑΒΓ") {
		t.Fatalf("expected a bfrange to map three codes, got %q", text)
	}
}

func TestFormXObjectContributesText(t *testing.T) {
	var b builder
	catalog := b.add("<</Type/Catalog/Pages 2 0 R>>")
	b.add("<</Type/Pages/Kids[3 0 R]/Count 1>>")
	b.add("<</Type/Page/Parent 2 0 R/MediaBox[0 0 612 792]" +
		"/Resources<</Font<</F1 5 0 R>>/XObject<</X1 6 0 R>>>>/Contents 4 0 R>>")
	b.addStream("", `BT /F1 12 Tf 72 700 Td (page text) Tj ET /X1 Do`)
	b.add("<</Type/Font/Subtype/Type1/BaseFont/Helvetica/Encoding/WinAnsiEncoding>>")
	b.addStream("/Type/XObject/Subtype/Form/BBox[0 0 612 792]",
		`BT /F1 12 Tf 72 600 Td (form text) Tj ET`)

	text := extractText(t, b.build(catalog))
	if !strings.Contains(text, "page text") || !strings.Contains(text, "form text") {
		t.Fatalf("expected text from both the page and the form, got %q", text)
	}
}

func TestInlineImageDataIsSkipped(t *testing.T) {
	// The bytes between ID and EI are raw image data. A tokenizer that reads
	// them as syntax produces garbage operators and can swallow later text.
	content := "BT /F1 12 Tf 72 700 Td (before) Tj ET\n" +
		"BI /W 2 /H 2 /BPC 8 /CS /G ID \x00(Tj garbage\xff\xfe EI\n" +
		"BT /F1 12 Tf 72 680 Td (after) Tj ET"
	text := extractText(t, simplePDF(t, content, ""))
	if !strings.Contains(text, "before") || !strings.Contains(text, "after") {
		t.Fatalf("inline image data disturbed extraction: %q", text)
	}
	if strings.Contains(text, "garbage") {
		t.Fatalf("inline image data was interpreted as text: %q", text)
	}
}

func TestRecoversFromBrokenCrossReference(t *testing.T) {
	document := simplePDF(t, `BT /F1 12 Tf 72 700 Td (recovered) Tj ET`, "")
	// Corrupt every offset in the cross-reference table. A reader that trusts
	// it finds nothing; scanning for object headers recovers the document.
	broken := bytes.Replace(document, []byte("0000000009"), []byte("0000000999"), -1)
	index := bytes.Index(broken, []byte("startxref"))
	if index < 0 {
		t.Fatal("test fixture has no startxref")
	}
	broken = append(broken[:index], []byte("startxref\n999999\n%%EOF\n")...)

	if text := extractText(t, broken); !strings.Contains(text, "recovered") {
		t.Fatalf("expected reconstruction to recover the text, got %q", text)
	}
}

func TestObjectStreamsAndXrefStreams(t *testing.T) {
	// Objects packed into an object stream, indexed by a cross-reference
	// stream: the layout every modern producer emits.
	packed := "<</Type/Catalog/Pages 2 0 R>> <</Type/Pages/Kids[3 0 R]/Count 1>> " +
		"<</Type/Page/Parent 2 0 R/MediaBox[0 0 612 792]" +
		"/Resources<</Font<</F1 5 0 R>>>>/Contents 4 0 R>> " +
		"<</Type/Font/Subtype/Type1/BaseFont/Helvetica/Encoding/WinAnsiEncoding>>"
	header := "1 0 2 30 3 68 5 180 "
	body := header + packed
	// Recompute the offsets so they point at each object inside the payload.
	offsets := []int{0, 30, 68, 180}
	parts := []string{
		"<</Type/Catalog/Pages 2 0 R>>",
		"<</Type/Pages/Kids[3 0 R]/Count 1>>",
		"<</Type/Page/Parent 2 0 R/MediaBox[0 0 612 792]/Resources<</Font<</F1 5 0 R>>>>/Contents 4 0 R>>",
		"<</Type/Font/Subtype/Type1/BaseFont/Helvetica/Encoding/WinAnsiEncoding>>",
	}
	var payload strings.Builder
	for index, part := range parts {
		for payload.Len() < offsets[index] {
			payload.WriteByte(' ')
		}
		payload.WriteString(part)
	}
	body = header + payload.String()
	first := len(header)

	var out bytes.Buffer
	out.WriteString("%PDF-1.7\n")
	objectOffsets := map[int]int{}

	objectOffsets[4] = out.Len()
	stream := `BT /F1 12 Tf 72 700 Td (object stream text) Tj ET`
	fmt.Fprintf(&out, "4 0 obj\n<</Length %d>>\nstream\n%s\nendstream\nendobj\n", len(stream), stream)

	objectOffsets[6] = out.Len()
	fmt.Fprintf(&out, "6 0 obj\n<</Type/ObjStm/N 4/First %d/Length %d>>\nstream\n%s\nendstream\nendobj\n",
		first, len(body), body)

	// A cross-reference stream with three one-byte-wide fields.
	xrefAt := out.Len()
	entries := []byte{
		0, 0, 0, // object 0, free
		2, 6, 0, // object 1 lives in stream 6 at slot 0
		2, 6, 1,
		2, 6, 2,
		1, byte(objectOffsets[4]), 0,
		2, 6, 3,
		1, byte(objectOffsets[6]), 0,
	}
	fmt.Fprintf(&out, "7 0 obj\n<</Type/XRef/Size 8/W[1 1 1]/Root 1 0 R/Length %d>>\nstream\n",
		len(entries))
	out.Write(entries)
	out.WriteString("\nendstream\nendobj\n")
	fmt.Fprintf(&out, "startxref\n%d\n%%%%EOF\n", xrefAt)

	if text := extractText(t, out.Bytes()); !strings.Contains(text, "object stream text") {
		t.Fatalf("expected text from an object stream document, got %q", text)
	}
}

func TestReportsImageOnlyDocument(t *testing.T) {
	var b builder
	catalog := b.add("<</Type/Catalog/Pages 2 0 R>>")
	b.add("<</Type/Pages/Kids[3 0 R]/Count 1>>")
	b.add("<</Type/Page/Parent 2 0 R/MediaBox[0 0 612 792]" +
		"/Resources<</XObject<</X1 5 0 R>>>>/Contents 4 0 R>>")
	b.addStream("", "q 612 0 0 792 0 0 cm /X1 Do Q")
	b.addStream("/Type/XObject/Subtype/Image/Width 10/Height 10"+
		"/ColorSpace/DeviceGray/BitsPerComponent 8", strings.Repeat("\x00", 100))

	result, err := Extract(b.build(catalog), Options{})
	if err != nil {
		t.Fatalf("Extract returned an error: %v", err)
	}
	if !result.ImageOnly {
		t.Fatal("a page of images with no text should be reported as image-only")
	}
}

func TestPasswordProtectedDocumentIsReported(t *testing.T) {
	// An encryption dictionary whose /U cannot be produced by the empty user
	// password means the file genuinely needs one.
	var b builder
	catalog := b.add("<</Type/Catalog/Pages 2 0 R>>")
	b.add("<</Type/Pages/Kids[3 0 R]/Count 1>>")
	b.add("<</Type/Page/Parent 2 0 R/MediaBox[0 0 612 792]/Contents 4 0 R>>")
	b.addStream("", "BT ET")
	b.add("<</Filter/Standard/V 5/R 6/Length 256" +
		"/O <" + strings.Repeat("aa", 48) + ">" +
		"/U <" + strings.Repeat("bb", 48) + ">" +
		"/UE <" + strings.Repeat("cc", 32) + ">/P -4>>")

	document := b.build(catalog)
	document = bytes.Replace(document, []byte("/Root 1 0 R"), []byte("/Root 1 0 R/Encrypt 5 0 R"), 1)

	_, err := Extract(document, Options{})
	if err == nil || !strings.Contains(err.Error(), "password") {
		t.Fatalf("expected a password-protected report, got %v", err)
	}
}

func TestMalformedInputNeverPanics(t *testing.T) {
	valid := simplePDF(t, `BT /F1 12 Tf 72 700 Td (text) Tj ET`, "")
	cases := [][]byte{
		nil,
		[]byte("not a pdf at all"),
		[]byte("%PDF-1.4\n"),
		[]byte("%PDF-1.4\n1 0 obj\n<</Length 99999>>\nstream\nshort"),
		[]byte("%PDF-1.4\n1 0 obj\n[[[[[[[[[[[[[[[[[[[[\nendobj\n"),
		[]byte("%PDF-1.4\n1 0 obj\n<</A 1 0 R>>\nendobj\ntrailer<</Root 1 0 R>>"),
		valid[:len(valid)/2],
		valid[:10],
		bytes.Replace(valid, []byte("/MediaBox[0 0 612 792]"), []byte("/MediaBox[0 0 0 0]"), 1),
		bytes.Replace(valid, []byte("Length"), []byte("Lengtx"), -1),
	}
	for index, document := range cases {
		// A panic here would take down the server that called it, so the only
		// acceptable outcomes are text or an error.
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Fatalf("case %d panicked: %v", index, recovered)
				}
			}()
			_, _ = Extract(document, Options{})
		}()
	}
}

func TestSelfReferentialObjectsTerminate(t *testing.T) {
	document := []byte("%PDF-1.4\n" +
		"1 0 obj\n<</Type/Catalog/Pages 2 0 R>>\nendobj\n" +
		"2 0 obj\n<</Type/Pages/Kids[2 0 R 3 0 R]/Count 1>>\nendobj\n" +
		"3 0 obj\n<</Type/Page/Parent 2 0 R/Contents 3 0 R>>\nendobj\n" +
		"trailer<</Root 1 0 R>>\n")
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = Extract(document, Options{})
	}()
	<-done
}

func TestExtractedCharacterCount(t *testing.T) {
	result, err := Extract(simplePDF(t, `BT /F1 12 Tf 72 700 Td (abcdef) Tj ET`, ""), Options{})
	if err != nil {
		t.Fatalf("Extract returned an error: %v", err)
	}
	if result.CharacterCount() != 6 {
		t.Fatalf("expected 6 characters, got %d", result.CharacterCount())
	}
}
