package sqlq

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// RenderOptions control output.
type RenderOptions struct {
	Format   string // table (default) | csv | json | md
	MaxWidth int    // table: truncate cells to this many characters (0 = no limit)
	MaxRows  int    // table: show at most this many rows (0 = all)
	Header   bool   // csv: write the header row (default true via DefaultRender)
}

// DefaultRender is the CLI default.
var DefaultRender = RenderOptions{Format: "table", MaxWidth: 60, MaxRows: 1000, Header: true}

// Formats accepted by Render.
var Formats = []string{"table", "csv", "json", "md"}

// ValidFormat reports whether f is a known output format.
func ValidFormat(f string) bool {
	for _, x := range Formats {
		if x == f || f == "markdown" {
			return true
		}
	}
	return false
}

func cellText(v any) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "", false
	case int64:
		return strconv.FormatInt(x, 10), true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case bool:
		if x {
			return "1", true
		}
		return "0", true
	case string:
		return x, true
	default:
		return fmt.Sprint(x), true
	}
}

func numeric(r *Result, col int) bool {
	any := false
	for _, row := range r.Rows {
		switch row[col].(type) {
		case nil:
		case int64, float64:
			any = true
		default:
			return false
		}
	}
	return any
}

// Render writes one result set.
func Render(w io.Writer, r *Result, o RenderOptions) error {
	switch o.Format {
	case "", "table":
		return renderTable(w, r, o)
	case "csv":
		cw := csv.NewWriter(w)
		if o.Header {
			if err := cw.Write(r.Columns); err != nil {
				return err
			}
		}
		rec := make([]string, len(r.Columns))
		for _, row := range r.Rows {
			for i, v := range row {
				rec[i], _ = cellText(v)
			}
			if err := cw.Write(rec); err != nil {
				return err
			}
		}
		cw.Flush()
		return cw.Error()
	case "json":
		return renderJSON(w, r)
	case "md", "markdown":
		return renderMD(w, r)
	default:
		return fmt.Errorf("unknown format %q (want %s)", o.Format, strings.Join(Formats, "|"))
	}
}

// RenderAll writes several result sets; table/md sets are separated by a blank line, json
// sets are written one array per line group.
func RenderAll(w io.Writer, rs []*Result, o RenderOptions) error {
	for i, r := range rs {
		if i > 0 {
			fmt.Fprintln(w)
		}
		if err := Render(w, r, o); err != nil {
			return err
		}
	}
	return nil
}

func renderJSON(w io.Writer, r *Result) error {
	keys := make([][]byte, len(r.Columns))
	seen := map[string]int{}
	for i, c := range r.Columns {
		k := c
		if n := seen[c]; n > 0 {
			k = fmt.Sprintf("%s_%d", c, n+1)
		}
		seen[c]++
		keys[i], _ = json.Marshal(k)
	}
	var b strings.Builder
	b.WriteString("[")
	for ri, row := range r.Rows {
		if ri > 0 {
			b.WriteString(",")
		}
		b.WriteString("\n  {")
		for i, v := range row {
			if i > 0 {
				b.WriteString(", ")
			}
			b.Write(keys[i])
			b.WriteString(": ")
			var vb []byte
			if f, ok := v.(float64); ok {
				vb = []byte(strconv.FormatFloat(f, 'f', -1, 64))
			} else {
				vb, _ = json.Marshal(v)
			}
			b.Write(vb)
		}
		b.WriteString("}")
	}
	if len(r.Rows) > 0 {
		b.WriteString("\n")
	}
	b.WriteString("]\n")
	_, err := io.WriteString(w, b.String())
	return err
}

func renderMD(w io.Writer, r *Result) error {
	esc := strings.NewReplacer("|", `\|`, "\r\n", "<br>", "\n", "<br>")
	hdr := make([]string, len(r.Columns))
	for i, c := range r.Columns {
		hdr[i] = esc.Replace(c)
	}
	fmt.Fprintf(w, "| %s |\n", strings.Join(hdr, " | "))
	al := make([]string, len(r.Columns))
	for i := range al {
		al[i] = "---"
		if numeric(r, i) {
			al[i] = "---:"
		}
	}
	fmt.Fprintf(w, "|%s|\n", strings.Join(al, "|"))
	cells := make([]string, len(r.Columns))
	for _, row := range r.Rows {
		for i, v := range row {
			s, ok := cellText(v)
			if !ok {
				s = "NULL"
			}
			cells[i] = esc.Replace(s)
		}
		fmt.Fprintf(w, "| %s |\n", strings.Join(cells, " | "))
	}
	return nil
}

func truncate(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, s)
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	if max == 1 {
		return "…"
	}
	rs := []rune(s)
	return string(rs[:max-1]) + "…"
}

func renderTable(w io.Writer, r *Result, o RenderOptions) error {
	n := len(r.Columns)
	right := make([]bool, n)
	width := make([]int, n)
	for i, c := range r.Columns {
		right[i] = numeric(r, i)
		width[i] = utf8.RuneCountInString(truncate(c, o.MaxWidth))
	}
	shown := r.Rows
	if o.MaxRows > 0 && len(shown) > o.MaxRows {
		shown = shown[:o.MaxRows]
	}
	cells := make([][]string, len(shown))
	for ri, row := range shown {
		cells[ri] = make([]string, n)
		for i, v := range row {
			s, ok := cellText(v)
			if !ok {
				s = "NULL"
			}
			s = truncate(s, o.MaxWidth)
			cells[ri][i] = s
			if l := utf8.RuneCountInString(s); l > width[i] {
				width[i] = l
			}
		}
	}
	var b strings.Builder
	line := func(vals []string) {
		for i, v := range vals {
			if i > 0 {
				b.WriteString("  ")
			}
			pad := strings.Repeat(" ", width[i]-utf8.RuneCountInString(v))
			if right[i] {
				b.WriteString(pad + v)
			} else if i == n-1 {
				b.WriteString(v)
			} else {
				b.WriteString(v + pad)
			}
		}
		b.WriteString("\n")
	}
	hdr := make([]string, n)
	dash := make([]string, n)
	for i, c := range r.Columns {
		hdr[i] = truncate(c, o.MaxWidth)
		dash[i] = strings.Repeat("-", width[i])
	}
	line(hdr)
	line(dash)
	for _, c := range cells {
		line(c)
	}
	switch {
	case len(shown) < len(r.Rows):
		fmt.Fprintf(&b, "(%d rows; showing first %d — use --max-rows 0 or --format csv for all)\n", len(r.Rows), len(shown))
	case len(r.Rows) == 1:
		b.WriteString("(1 row)\n")
	default:
		fmt.Fprintf(&b, "(%d rows)\n", len(r.Rows))
	}
	_, err := io.WriteString(w, b.String())
	return err
}
