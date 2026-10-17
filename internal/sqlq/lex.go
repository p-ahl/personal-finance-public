package sqlq

import "strings"

// token kinds produced by scan
const (
	tCode = iota
	tString
	tComment
)

// scan walks SQL text and calls fn for each run of code, quoted literal/identifier or comment.
func scan(src string, fn func(kind int, text string)) {
	i, start := 0, 0
	flush := func(end int) {
		if end > start {
			fn(tCode, src[start:end])
		}
	}
	for i < len(src) {
		c := src[i]
		switch {
		case c == '\'' || c == '"' || c == '`' || c == '[':
			flush(i)
			closer := c
			if c == '[' {
				closer = ']'
			}
			j := i + 1
			for j < len(src) {
				if src[j] == closer {
					if closer != ']' && j+1 < len(src) && src[j+1] == closer { // doubled quote
						j += 2
						continue
					}
					break
				}
				j++
			}
			if j < len(src) {
				j++
			}
			fn(tString, src[i:j])
			i, start = j, j
		case c == '-' && i+1 < len(src) && src[i+1] == '-':
			flush(i)
			j := strings.IndexByte(src[i:], '\n')
			if j < 0 {
				j = len(src)
			} else {
				j += i
			}
			fn(tComment, src[i:j])
			i, start = j, j
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			flush(i)
			j := strings.Index(src[i+2:], "*/")
			if j < 0 {
				j = len(src)
			} else {
				j += i + 4
			}
			fn(tComment, src[i:j])
			i, start = j, j
		default:
			i++
		}
	}
	flush(len(src))
}

// Split splits a script into statements on top-level semicolons, keeping quoted text and
// comments intact. Comment-only fragments are dropped. (CREATE TRIGGER bodies are not
// supported, which is fine for a read-only query tool.)
func Split(src string) []string {
	var out []string
	var cur strings.Builder
	hasCode := false
	emit := func() {
		if hasCode {
			out = append(out, strings.TrimSpace(cur.String()))
		}
		cur.Reset()
		hasCode = false
	}
	scan(src, func(kind int, text string) {
		if kind != tCode {
			cur.WriteString(text)
			if kind == tString {
				hasCode = true
			}
			return
		}
		for {
			k := strings.IndexByte(text, ';')
			if k < 0 {
				break
			}
			cur.WriteString(text[:k])
			if strings.TrimSpace(text[:k]) != "" {
				hasCode = true
			}
			emit()
			text = text[k+1:]
		}
		cur.WriteString(text)
		if strings.TrimSpace(text) != "" {
			hasCode = true
		}
	})
	emit()
	return out
}

// ParamNames returns the distinct named parameters (:name, @name, $name) in a statement, in
// order of first use, ignoring quoted text and comments.
func ParamNames(stmt string) []string {
	var out []string
	seen := map[string]bool{}
	scan(stmt, func(kind int, text string) {
		if kind != tCode {
			return
		}
		for i := 0; i < len(text); i++ {
			c := text[i]
			if c != ':' && c != '@' && c != '$' {
				continue
			}
			j := i + 1
			for j < len(text) && (isIdent(text[j]) || (j > i+1 && text[j] >= '0' && text[j] <= '9')) {
				j++
			}
			if j > i+1 && !(text[i+1] >= '0' && text[i+1] <= '9') {
				n := text[i+1 : j]
				if !seen[n] {
					seen[n] = true
					out = append(out, n)
				}
			}
			i = j - 1
		}
	})
	return out
}

func isIdent(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}
