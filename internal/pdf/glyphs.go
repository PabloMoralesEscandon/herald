package pdf

import (
	"strconv"
	"strings"
)

// This file maps PostScript glyph names to text.
//
// It is what makes text from LaTeX PDFs readable. A pdfTeX document usually
// embeds subset Type 1 fonts with a custom /Differences encoding and no
// ToUnicode map at all, so the only description of what a byte means is a
// glyph name such as /fi or /quoteright. Without this table those documents
// extract as mojibake, which is the classic failure of naive PDF text
// extraction.

// asciiGlyphNames are the names for byte values 32 to 126, shared by every
// standard encoding apart from two positions noted in the encoding tables.
var asciiGlyphNames = [...]string{
	"space", "exclam", "quotedbl", "numbersign", "dollar", "percent",
	"ampersand", "quotesingle", "parenleft", "parenright", "asterisk", "plus",
	"comma", "hyphen", "period", "slash", "zero", "one", "two", "three",
	"four", "five", "six", "seven", "eight", "nine", "colon", "semicolon",
	"less", "equal", "greater", "question", "at", "A", "B", "C", "D", "E",
	"F", "G", "H", "I", "J", "K", "L", "M", "N", "O", "P", "Q", "R", "S", "T",
	"U", "V", "W", "X", "Y", "Z", "bracketleft", "backslash", "bracketright",
	"asciicircum", "underscore", "grave", "a", "b", "c", "d", "e", "f", "g",
	"h", "i", "j", "k", "l", "m", "n", "o", "p", "q", "r", "s", "t", "u", "v",
	"w", "x", "y", "z", "braceleft", "bar", "braceright", "asciitilde",
}

// winAnsiHigh is the upper half of WinAnsiEncoding, which is Windows code page
// 1252 rather than Latin-1.
var winAnsiHigh = [...]string{
	"Euro", "", "quotesinglbase", "florin", "quotedblbase", "ellipsis",
	"dagger", "daggerdbl", "circumflex", "perthousand", "Scaron",
	"guilsinglleft", "OE", "", "Zcaron", "", "", "quoteleft", "quoteright",
	"quotedblleft", "quotedblright", "bullet", "endash", "emdash", "tilde",
	"trademark", "scaron", "guilsinglright", "oe", "", "zcaron", "Ydieresis",
	"space", "exclamdown", "cent", "sterling", "currency", "yen", "brokenbar",
	"section", "dieresis", "copyright", "ordfeminine", "guillemotleft",
	"logicalnot", "hyphen", "registered", "macron", "degree", "plusminus",
	"twosuperior", "threesuperior", "acute", "mu", "paragraph", "periodcentered",
	"cedilla", "onesuperior", "ordmasculine", "guillemotright", "onequarter",
	"onehalf", "threequarters", "questiondown", "Agrave", "Aacute",
	"Acircumflex", "Atilde", "Adieresis", "Aring", "AE", "Ccedilla", "Egrave",
	"Eacute", "Ecircumflex", "Edieresis", "Igrave", "Iacute", "Icircumflex",
	"Idieresis", "Eth", "Ntilde", "Ograve", "Oacute", "Ocircumflex", "Otilde",
	"Odieresis", "multiply", "Oslash", "Ugrave", "Uacute", "Ucircumflex",
	"Udieresis", "Yacute", "Thorn", "germandbls", "agrave", "aacute",
	"acircumflex", "atilde", "adieresis", "aring", "ae", "ccedilla", "egrave",
	"eacute", "ecircumflex", "edieresis", "igrave", "iacute", "icircumflex",
	"idieresis", "eth", "ntilde", "ograve", "oacute", "ocircumflex", "otilde",
	"odieresis", "divide", "oslash", "ugrave", "uacute", "ucircumflex",
	"udieresis", "yacute", "thorn", "ydieresis",
}

// macRomanHigh is the upper half of MacRomanEncoding.
var macRomanHigh = [...]string{
	"Adieresis", "Aring", "Ccedilla", "Eacute", "Ntilde", "Odieresis",
	"Udieresis", "aacute", "agrave", "acircumflex", "adieresis", "atilde",
	"aring", "ccedilla", "eacute", "egrave", "ecircumflex", "edieresis",
	"iacute", "igrave", "icircumflex", "idieresis", "ntilde", "oacute",
	"ograve", "ocircumflex", "odieresis", "otilde", "uacute", "ugrave",
	"ucircumflex", "udieresis", "dagger", "degree", "cent", "sterling",
	"section", "bullet", "paragraph", "germandbls", "registered", "copyright",
	"trademark", "acute", "dieresis", "notequal", "AE", "Oslash", "infinity",
	"plusminus", "lessequal", "greaterequal", "yen", "mu", "partialdiff",
	"summation", "product", "pi", "integral", "ordfeminine", "ordmasculine",
	"Omega", "ae", "oslash", "questiondown", "exclamdown", "logicalnot",
	"radical", "florin", "approxequal", "Delta", "guillemotleft",
	"guillemotright", "ellipsis", "space", "Agrave", "Atilde", "Otilde", "OE",
	"oe", "endash", "emdash", "quotedblleft", "quotedblright", "quoteleft",
	"quoteright", "divide", "lozenge", "ydieresis", "Ydieresis", "fraction",
	"currency", "guilsinglleft", "guilsinglright", "fi", "fl", "daggerdbl",
	"periodcentered", "quotesinglbase", "quotedblbase", "perthousand",
	"Acircumflex", "Ecircumflex", "Aacute", "Edieresis", "Egrave", "Iacute",
	"Icircumflex", "Idieresis", "Igrave", "Oacute", "Ocircumflex", "apple",
	"Ograve", "Uacute", "Ucircumflex", "Ugrave", "dotlessi", "circumflex",
	"tilde", "macron", "breve", "dotaccent", "ring", "cedilla", "hungarumlaut",
	"ogonek", "caron",
}

// standardHigh is the upper half of StandardEncoding, which is sparse.
var standardHigh = map[int]string{
	161: "exclamdown", 162: "cent", 163: "sterling", 164: "fraction",
	165: "yen", 166: "florin", 167: "section", 168: "currency",
	169: "quotesingle", 170: "quotedblleft", 171: "guillemotleft",
	172: "guilsinglleft", 173: "guilsinglright", 174: "fi", 175: "fl",
	177: "endash", 178: "dagger", 179: "daggerdbl", 180: "periodcentered",
	182: "paragraph", 183: "bullet", 184: "quotesinglbase",
	185: "quotedblbase", 186: "quotedblright", 187: "guillemotright",
	188: "ellipsis", 189: "perthousand", 191: "questiondown", 193: "grave",
	194: "acute", 195: "circumflex", 196: "tilde", 197: "macron",
	198: "breve", 199: "dotaccent", 200: "dieresis", 202: "ring",
	203: "cedilla", 205: "hungarumlaut", 206: "ogonek", 207: "caron",
	208: "emdash", 225: "AE", 227: "ordfeminine", 232: "Lslash",
	233: "Oslash", 234: "OE", 235: "ordmasculine", 241: "ae",
	245: "dotlessi", 248: "lslash", 249: "oslash", 250: "oe",
	251: "germandbls",
}

// pdfDocHigh is PDFDocEncoding's upper half, used by document metadata rather
// than by page content.
var symbolNames = map[string]rune{
	// Typography.
	"quoteleft": '‘', "quoteright": '’', "quotedblleft": '“',
	"quotedblright": '”', "quotesinglbase": '‚', "quotedblbase": '„',
	"endash": '–', "emdash": '—', "bullet": '•',
	"dagger": '†', "daggerdbl": '‡', "ellipsis": '…',
	"perthousand": '‰', "fraction": '⁄', "guilsinglleft": '‹',
	"guilsinglright": '›', "guillemotleft": '«', "guillemotright": '»',
	"trademark": '™', "copyright": '©', "registered": '®',
	"section": '§', "paragraph": '¶', "periodcentered": '·',
	"florin": 'ƒ', "Euro": '€', "currency": '¤',
	"sterling": '£', "yen": '¥', "cent": '¢',
	"brokenbar": '¦', "exclamdown": '¡', "questiondown": '¿',
	"ordfeminine": 'ª', "ordmasculine": 'º', "degree": '°',
	"minute": '′', "second": '″', "apple": '',
	"lozenge": '◊', "space": ' ', "nbspace": ' ',

	// Ligatures. LaTeX output is full of these and they are the single most
	// visible symptom when glyph names are not mapped.
	"fi": 'ﬁ', "fl": 'ﬂ', "ff": 'ﬀ', "ffi": 'ﬃ',
	"ffl": 'ﬄ', "dotlessi": 'ı', "dotlessj": 'ȷ',

	// Accents as standalone glyphs.
	"acute": '´', "grave": '`', "circumflex": 'ˆ', "tilde": '˜',
	"macron": '¯', "breve": '˘', "dotaccent": '˙',
	"dieresis": '¨', "ring": '˚', "cedilla": '¸',
	"hungarumlaut": '˝', "ogonek": '˛', "caron": 'ˇ',

	// Mathematics and arrows, common in paper bodies.
	"minus": '−', "plusminus": '±', "multiply": '×',
	"divide": '÷', "notequal": '≠', "lessequal": '≤',
	"greaterequal": '≥', "approxequal": '≈', "equivalence": '≡',
	"infinity": '∞', "integral": '∫', "partialdiff": '∂',
	"summation": '∑', "product": '∏', "radical": '√',
	"proportional": '∝', "element": '∈', "notelement": '∉',
	"intersection": '∩', "union": '∪', "logicalnot": '¬',
	"logicaland": '∧', "logicalor": '∨', "existential": '∃',
	"universal": '∀', "emptyset": '∅', "gradient": '∇',
	"arrowleft": '←', "arrowup": '↑', "arrowright": '→',
	"arrowdown": '↓', "arrowboth": '↔', "arrowdblright": '⇒',
	"arrowdblboth": '⇔', "similar": '∼', "congruent": '≅',
	"propersubset": '⊂', "propersuperset": '⊃',
	"reflexsubset": '⊆', "reflexsuperset": '⊇',
	"angle": '∠', "perpendicular": '⊥', "therefore": '∴',
	"asteriskmath": '∗', "circlemultiply": '⊗', "circleplus": '⊕',
	"dotmath": '⋅', "openbullet": '◦',

	// Superscripts and fractions.
	"onesuperior": '¹', "twosuperior": '²', "threesuperior": '³',
	"onequarter": '¼', "onehalf": '½', "threequarters": '¾',

	// Greek, which appears constantly in scientific text.
	"Alpha": 'Α', "Beta": 'Β', "Gamma": 'Γ', "Delta": 'Δ',
	"Epsilon": 'Ε', "Zeta": 'Ζ', "Eta": 'Η', "Theta": 'Θ',
	"Iota": 'Ι', "Kappa": 'Κ', "Lambda": 'Λ', "Mu": 'Μ',
	"Nu": 'Ν', "Xi": 'Ξ', "Omicron": 'Ο', "Pi": 'Π',
	"Rho": 'Ρ', "Sigma": 'Σ', "Tau": 'Τ', "Upsilon": 'Υ',
	"Phi": 'Φ', "Chi": 'Χ', "Psi": 'Ψ', "Omega": 'Ω',
	"alpha": 'α', "beta": 'β', "gamma": 'γ', "delta": 'δ',
	"epsilon": 'ε', "zeta": 'ζ', "eta": 'η', "theta": 'θ',
	"iota": 'ι', "kappa": 'κ', "lambda": 'λ', "mu": 'μ',
	"nu": 'ν', "xi": 'ξ', "omicron": 'ο', "pi": 'π',
	"rho": 'ρ', "sigma": 'σ', "sigma1": 'ς', "tau": 'τ',
	"upsilon": 'υ', "phi": 'φ', "chi": 'χ', "psi": 'ψ',
	"omega": 'ω', "theta1": 'ϑ', "phi1": 'ϕ', "epsilon1": 'ϵ',
	"omega1": 'ϖ', "rho1": 'ϱ',

	// Latin letters with diacritics that are not simply "letter+accent".
	"germandbls": 'ß', "AE": 'Æ', "ae": 'æ', "OE": 'Œ',
	"oe": 'œ', "Oslash": 'Ø', "oslash": 'ø', "Lslash": 'Ł',
	"lslash": 'ł', "Eth": 'Ð', "eth": 'ð', "Thorn": 'Þ',
	"thorn": 'þ', "Scaron": 'Š', "scaron": 'š',
	"Zcaron": 'Ž', "zcaron": 'ž', "Ydieresis": 'Ÿ',
	"Aring": 'Å', "aring": 'å', "Ccedilla": 'Ç',
	"ccedilla": 'ç', "Ntilde": 'Ñ', "ntilde": 'ñ',
}

// accentedGlyphs covers the "letter + accent name" pattern mechanically, which
// is how the Latin-1 range is spelled in every standard encoding.
var accentSuffixes = map[string]rune{
	"grave": '̀', "acute": '́', "circumflex": '̂',
	"tilde": '̃', "macron": '̄', "breve": '̆',
	"dotaccent": '̇', "dieresis": '̈', "ring": '̊',
	"hungarumlaut": '̋', "caron": '̌', "cedilla": '̧',
	"ogonek": '̨',
}

// precomposed maps the base letter and combining mark to a single character,
// covering the Latin-1 and Latin Extended-A letters papers actually use.
var precomposed = map[rune]map[rune]rune{
	'A': {'̀': 'À', '́': 'Á', '̂': 'Â', '̃': 'Ã', '̈': 'Ä', '̊': 'Å', '̆': 'Ă', '̨': 'Ą'},
	'C': {'́': 'Ć', '̂': 'Ĉ', '̌': 'Č', '̧': 'Ç'},
	'E': {'̀': 'È', '́': 'É', '̂': 'Ê', '̈': 'Ë', '̌': 'Ě', '̨': 'Ę'},
	'I': {'̀': 'Ì', '́': 'Í', '̂': 'Î', '̈': 'Ï'},
	'N': {'́': 'Ń', '̃': 'Ñ', '̌': 'Ň'},
	'O': {'̀': 'Ò', '́': 'Ó', '̂': 'Ô', '̃': 'Õ', '̈': 'Ö', '̋': 'Ő'},
	'R': {'̌': 'Ř'},
	'S': {'́': 'Ś', '̌': 'Š', '̧': 'Ş'},
	'U': {'̀': 'Ù', '́': 'Ú', '̂': 'Û', '̈': 'Ü', '̊': 'Ů', '̋': 'Ű'},
	'Y': {'́': 'Ý', '̈': 'Ÿ'},
	'Z': {'́': 'Ź', '̇': 'Ż', '̌': 'Ž'},
	'a': {'̀': 'à', '́': 'á', '̂': 'â', '̃': 'ã', '̈': 'ä', '̊': 'å', '̆': 'ă', '̨': 'ą'},
	'c': {'́': 'ć', '̂': 'ĉ', '̌': 'č', '̧': 'ç'},
	'e': {'̀': 'è', '́': 'é', '̂': 'ê', '̈': 'ë', '̌': 'ě', '̨': 'ę'},
	'g': {'̆': 'ğ'},
	'i': {'̀': 'ì', '́': 'í', '̂': 'î', '̈': 'ï'},
	'l': {'́': 'ĺ'},
	'n': {'́': 'ń', '̃': 'ñ', '̌': 'ň'},
	'o': {'̀': 'ò', '́': 'ó', '̂': 'ô', '̃': 'õ', '̈': 'ö', '̋': 'ő'},
	'r': {'̌': 'ř'},
	's': {'́': 'ś', '̌': 'š', '̧': 'ş'},
	't': {'̌': 'ť'},
	'u': {'̀': 'ù', '́': 'ú', '̂': 'û', '̈': 'ü', '̊': 'ů', '̋': 'ű'},
	'y': {'́': 'ý', '̈': 'ÿ'},
	'z': {'́': 'ź', '̇': 'ż', '̌': 'ž'},
}

// asciiByName inverts asciiGlyphNames once, so lookups are a map hit.
var asciiByName = func() map[string]rune {
	names := map[string]rune{}
	for index, name := range asciiGlyphNames {
		names[name] = rune(32 + index)
	}
	return names
}()

// glyphText converts a PostScript glyph name to the text it represents.
//
// It returns an empty string for a name it cannot interpret, which the caller
// treats as "no text here" rather than substituting a placeholder that would
// corrupt the extracted words.
func glyphText(name string) string {
	if name == "" || name == ".notdef" {
		return ""
	}
	// A variant suffix such as "a.sc" or "one.oldstyle" names the same
	// character in a different shape.
	if dot := strings.IndexByte(name, '.'); dot > 0 {
		name = name[:dot]
	}
	if r, ok := asciiByName[name]; ok {
		return string(r)
	}
	if r, ok := symbolNames[name]; ok {
		return string(r)
	}
	if text, ok := uniName(name); ok {
		return text
	}
	if text, ok := accentedName(name); ok {
		return text
	}
	// "a" through "z" and single characters are their own names in some subset
	// fonts, as are names built by concatenation such as "one_two".
	if strings.Contains(name, "_") {
		var out strings.Builder
		for _, part := range strings.Split(name, "_") {
			out.WriteString(glyphText(part))
		}
		return out.String()
	}
	if runes := []rune(name); len(runes) == 1 {
		return string(runes)
	}
	return ""
}

// uniName handles the algorithmic uniXXXX and uXXXXXX spellings.
func uniName(name string) (string, bool) {
	switch {
	case strings.HasPrefix(name, "uni") && len(name) >= 7 && (len(name)-3)%4 == 0:
		var out strings.Builder
		for index := 3; index+4 <= len(name); index += 4 {
			value, err := strconv.ParseUint(name[index:index+4], 16, 32)
			if err != nil {
				return "", false
			}
			out.WriteRune(rune(value))
		}
		return out.String(), true
	case strings.HasPrefix(name, "u") && len(name) >= 5 && len(name) <= 7:
		value, err := strconv.ParseUint(name[1:], 16, 32)
		if err != nil {
			return "", false
		}
		return string(rune(value)), true
	}
	return "", false
}

// accentedName resolves names built as a base letter plus an accent, such as
// "eacute" or "ohungarumlaut".
func accentedName(name string) (string, bool) {
	for suffix, mark := range accentSuffixes {
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		base := name[:len(name)-len(suffix)]
		baseRunes := []rune(base)
		if len(baseRunes) != 1 {
			continue
		}
		if table, ok := precomposed[baseRunes[0]]; ok {
			if composed, ok := table[mark]; ok {
				return string(composed), true
			}
		}
		// Without a precomposed form, the base letter followed by the
		// combining mark is still correct text.
		return string(baseRunes[0]) + string(mark), true
	}
	return "", false
}

// baseEncoding returns the 256-entry glyph name table for a named encoding.
func baseEncoding(name Name) [256]string {
	var table [256]string
	for index, glyph := range asciiGlyphNames {
		table[32+index] = glyph
	}
	switch name {
	case "WinAnsiEncoding":
		for index, glyph := range winAnsiHigh {
			table[128+index] = glyph
		}
	case "MacRomanEncoding":
		table[39], table[96] = "quotesingle", "grave"
		for index, glyph := range macRomanHigh {
			table[128+index] = glyph
		}
	default: // StandardEncoding, and the fallback for anything unknown.
		table[39], table[96] = "quoteright", "quoteleft"
		for code, glyph := range standardHigh {
			table[code] = glyph
		}
	}
	return table
}
