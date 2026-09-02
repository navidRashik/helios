package opencode

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// A minimal JSONC editor, because the file being edited is the user's.
//
// opencode's global config is JSONC and the one on the machine this was
// written against holds thirty-odd comments explaining why each setting is
// what it is. Unmarshalling it into a map and marshalling it back would
// produce valid JSON, load correctly, and silently delete every one of them —
// a config edit is not permission to throw away the reasoning around it.
//
// So the entry is spliced in as text. Everything outside the few bytes
// inserted or removed comes through byte for byte: comments, key order,
// indentation, trailing commas, the lot.
//
// Deliberately not a JSONC parser. It does not build a value, validate the
// document, or report a type error. It answers one question — where is the
// plugin array and what strings are in it — and it has to be right about
// string and comment boundaries to answer it, which is all the scanner below
// does.

var errNoRootObject = errors.New("config is not a JSON object")

// span is a half-open byte range [start, end).
type span struct{ start, end int }

// scanner walks JSONC, skipping comments and tracking nesting.
type scanner struct {
	src   string
	i     int
	depth int // nesting of {} and [] combined
}

// skipTrivia advances past whitespace and both comment forms.
//
// An unterminated block comment consumes the rest of the file rather than
// erroring: opencode's own parser would reject the document anyway, and this
// one has nothing useful to say about it.
func (s *scanner) skipTrivia() {
	for s.i < len(s.src) {
		c := s.src[s.i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			s.i++
		case strings.HasPrefix(s.src[s.i:], "//"):
			if n := strings.IndexByte(s.src[s.i:], '\n'); n < 0 {
				s.i = len(s.src)
			} else {
				s.i += n + 1
			}
		case strings.HasPrefix(s.src[s.i:], "/*"):
			if n := strings.Index(s.src[s.i+2:], "*/"); n < 0 {
				s.i = len(s.src)
			} else {
				s.i += 2 + n + 2
			}
		default:
			return
		}
	}
}

// readString consumes a quoted string starting at s.i and returns its span,
// quotes included. s.i must be on the opening quote.
func (s *scanner) readString() (span, bool) {
	if s.i >= len(s.src) || s.src[s.i] != '"' {
		return span{}, false
	}
	start := s.i
	s.i++
	for s.i < len(s.src) {
		switch s.src[s.i] {
		case '\\':
			// Skips the escaped byte, which is what keeps a \" from being read
			// as the end of the string.
			s.i += 2
			continue
		case '"':
			s.i++
			return span{start, s.i}, true
		}
		s.i++
	}
	return span{}, false
}

// next advances one token, maintaining depth. Strings and comments are
// consumed whole so a brace inside either never counts.
func (s *scanner) next() bool {
	s.skipTrivia()
	if s.i >= len(s.src) {
		return false
	}
	switch s.src[s.i] {
	case '"':
		_, ok := s.readString()
		return ok
	case '{', '[':
		s.depth++
		s.i++
	case '}', ']':
		s.depth--
		s.i++
	default:
		s.i++
	}
	return true
}

// findPluginArray locates the top-level "plugin" array.
//
// Only at depth 1, so a "plugin" key nested inside some other object — an MCP
// server's own settings, say — is not mistaken for the real one.
func findPluginArray(src string) (open, close int, ok bool) {
	s := &scanner{src: src}
	for s.i < len(src) {
		s.skipTrivia()
		if s.i >= len(src) {
			break
		}
		if src[s.i] == '"' && s.depth == 1 {
			str, valid := s.readString()
			if !valid {
				return 0, 0, false
			}
			if src[str.start:str.end] != `"plugin"` {
				continue
			}
			// A key, not a value in some array: the next thing is a colon.
			s.skipTrivia()
			if s.i >= len(src) || src[s.i] != ':' {
				continue
			}
			s.i++
			s.skipTrivia()
			if s.i >= len(src) || src[s.i] != '[' {
				// "plugin" set to something that is not an array. Left alone
				// rather than replaced: it is not ours to reinterpret.
				return 0, 0, false
			}
			open = s.i
			end, found := matchBracket(src, open)
			if !found {
				return 0, 0, false
			}
			return open, end, true
		}
		if !s.next() {
			break
		}
	}
	return 0, 0, false
}

// matchBracket returns the index of the bracket closing the one at open.
func matchBracket(src string, open int) (int, bool) {
	s := &scanner{src: src, i: open}
	for s.i < len(src) {
		before := s.depth
		if !s.next() {
			return 0, false
		}
		if before == 1 && s.depth == 0 {
			return s.i - 1, true
		}
	}
	return 0, false
}

// arrayStrings returns the spans of the string elements directly inside the
// array, ignoring anything nested.
func arrayStrings(src string, open, close int) []span {
	var out []span
	s := &scanner{src: src, i: open + 1}
	for s.i < close {
		s.skipTrivia()
		if s.i >= close {
			break
		}
		if src[s.i] == '"' && s.depth == 0 {
			if str, ok := s.readString(); ok {
				out = append(out, str)
				continue
			}
			break
		}
		if !s.next() {
			break
		}
	}
	return out
}

// unquote decodes a JSON string literal, or returns "" when it cannot.
func unquote(src string, sp span) string {
	v, err := strconv.Unquote(src[sp.start:sp.end])
	if err != nil {
		return ""
	}
	return v
}

// findRootObject returns the index of the document's opening brace.
func findRootObject(src string) (int, bool) {
	s := &scanner{src: src}
	s.skipTrivia()
	if s.i >= len(src) || src[s.i] != '{' {
		return 0, false
	}
	return s.i, true
}

// hasPluginEntry reports whether the config already loads entry.
func hasPluginEntry(src, entry string) bool {
	open, close, ok := findPluginArray(src)
	if !ok {
		return false
	}
	for _, sp := range arrayStrings(src, open, close) {
		if unquote(src, sp) == entry {
			return true
		}
	}
	return false
}

// indentOf returns the whitespace prefix of the line containing i.
func indentOf(src string, i int) string {
	start := strings.LastIndexByte(src[:i], '\n') + 1
	return src[start : start+len(src[start:i])-len(strings.TrimLeft(src[start:i], " \t"))]
}

// addPluginEntry splices entry into the config's plugin array, preserving
// everything already in the file.
//
// Inserted first in the array rather than appended, and that placement is
// load-bearing. opencode runs plugins in the order they are listed and they
// share one mutable output object, so a plugin that rewrites a tool's
// arguments changes what a later one observes. helios only reports, and it
// should report what the model asked for rather than what the last plugin in
// the chain turned it into.
func addPluginEntry(src, entry string) (string, bool, error) {
	if entry == "" {
		return src, false, errors.New("empty plugin entry")
	}
	if hasPluginEntry(src, entry) {
		return src, false, nil
	}
	quoted := strconv.Quote(entry)

	if open, close, ok := findPluginArray(src); ok {
		// An array with nothing in it takes the entry alone; anything else
		// takes it plus a comma, on the indentation the first element uses.
		existing := arrayStrings(src, open, close)
		if len(existing) == 0 && strings.TrimSpace(src[open+1:close]) == "" {
			return src[:open+1] + quoted + src[close:], true, nil
		}
		indent := indentOf(src, existing[0].start)
		if indent == "" {
			indent = indentOf(src, open) + "  "
		}
		insert := "\n" + indent + quoted + ","
		return src[:open+1] + insert + src[open+1:], true, nil
	}

	// No plugin key at all. Added at the top of the root object, where a
	// reader looking for what helios did to their config will see it.
	brace, ok := findRootObject(src)
	if !ok {
		return src, false, errNoRootObject
	}
	indent := indentOf(src, brace) + "  "
	insert := fmt.Sprintf("\n%s\"plugin\": [%s],", indent, quoted)
	// An empty object has no following key for the comma to separate.
	if strings.TrimSpace(src[brace+1:]) == "}" {
		insert = fmt.Sprintf("\n%s\"plugin\": [%s]\n", indent, quoted)
	}
	return src[:brace+1] + insert + src[brace+1:], true, nil
}

// removePluginEntry deletes entry from the plugin array and leaves every other
// element, and every comment, where it was.
//
// The whole line goes when the entry is alone on it, which is how it was
// written; otherwise only the element and its separating comma. A comment on
// its own line above the entry is left behind, because it may as easily belong
// to the element below it, and a wrongly deleted comment is not recoverable
// while a stray one is.
func removePluginEntry(src, entry string) (string, bool, error) {
	if entry == "" {
		return src, false, nil
	}
	open, close, ok := findPluginArray(src)
	if !ok {
		return src, false, nil
	}
	var target span
	found := false
	for _, sp := range arrayStrings(src, open, close) {
		if unquote(src, sp) == entry {
			target, found = sp, true
			break
		}
	}
	if !found {
		return src, false, nil
	}

	start, end := target.start, target.end
	// Take the comma that separated it, whichever side it is on.
	if j := skipTriviaFrom(src, end, close); j < close && src[j] == ',' {
		end = j + 1
	} else if j := prevNonTrivia(src, open+1, start); j >= 0 && src[j] == ',' {
		start = j
	}
	// Alone on its line: take the line.
	lineStart := strings.LastIndexByte(src[:start], '\n') + 1
	if strings.TrimSpace(src[lineStart:start]) == "" {
		if n := strings.IndexByte(src[end:], '\n'); n >= 0 && strings.TrimSpace(src[end:end+n]) == "" {
			start, end = lineStart, end+n+1
		}
	}
	return src[:start] + src[end:], true, nil
}

// skipTriviaFrom returns the next index at or after i holding neither
// whitespace nor a comment, bounded by limit.
func skipTriviaFrom(src string, i, limit int) int {
	s := &scanner{src: src[:limit], i: i}
	s.skipTrivia()
	return s.i
}

// prevNonTrivia scans backwards for the last non-whitespace byte before i.
//
// Comments are not skipped going backwards, and they do not need to be: the
// only thing this is asked is whether a comma sits between the previous
// element and this one, and JSONC has nowhere to put a comment that would
// change that answer without also putting whitespace between the two.
func prevNonTrivia(src string, low, i int) int {
	for j := i - 1; j >= low; j-- {
		switch src[j] {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			return j
		}
	}
	return -1
}
