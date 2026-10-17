// Package report renders financial statements from the ledger views. All arithmetic is done on
// integer minor units in SQL/Go; formatting is the last step.
package report

import (
	"encoding/csv"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/p-ahl/personal-finance/internal/money"
)

// Table is a simple rectangular result. Cells are pre-formatted strings; Right marks numeric
// columns (right-aligned in text output).
type Table struct {
	Title   string
	Headers []string
	Right   []bool
	Rows    [][]string
	Notes   []string
}

func (t *Table) Add(cells ...string) { t.Rows = append(t.Rows, cells) }

// Sep adds a separator row (rendered as dashes in text mode, skipped in csv).
func (t *Table) Sep() { t.Rows = append(t.Rows, nil) }

func (t *Table) Render(w io.Writer, format string) error {
	switch format {
	case "csv":
		cw := csv.NewWriter(w)
		if err := cw.Write(t.Headers); err != nil {
			return err
		}
		for _, r := range t.Rows {
			if r == nil {
				continue
			}
			if err := cw.Write(r); err != nil {
				return err
			}
		}
		cw.Flush()
		return cw.Error()
	case "md", "markdown":
		if t.Title != "" {
			fmt.Fprintf(w, "**%s**\n\n", t.Title)
		}
		fmt.Fprintf(w, "| %s |\n", strings.Join(t.Headers, " | "))
		al := make([]string, len(t.Headers))
		for i := range al {
			al[i] = "---"
			if i < len(t.Right) && t.Right[i] {
				al[i] = "---:"
			}
		}
		fmt.Fprintf(w, "|%s|\n", strings.Join(al, "|"))
		for _, r := range t.Rows {
			if r == nil {
				continue
			}
			fmt.Fprintf(w, "| %s |\n", strings.Join(r, " | "))
		}
		for _, n := range t.Notes {
			fmt.Fprintf(w, "\n%s\n", n)
		}
		fmt.Fprintln(w)
		return nil
	default:
		width := make([]int, len(t.Headers))
		for i, h := range t.Headers {
			width[i] = utf8.RuneCountInString(h)
		}
		for _, r := range t.Rows {
			for i, c := range r {
				if i < len(width) && utf8.RuneCountInString(c) > width[i] {
					width[i] = utf8.RuneCountInString(c)
				}
			}
		}
		line := func(cells []string) {
			parts := make([]string, len(width))
			for i := range width {
				c := ""
				if i < len(cells) {
					c = cells[i]
				}
				pad := strings.Repeat(" ", width[i]-utf8.RuneCountInString(c))
				if i < len(t.Right) && t.Right[i] {
					parts[i] = pad + c
				} else {
					parts[i] = c + pad
				}
			}
			fmt.Fprintln(w, strings.TrimRight(strings.Join(parts, "  "), " "))
		}
		if t.Title != "" {
			fmt.Fprintln(w, t.Title)
		}
		line(t.Headers)
		dash := make([]string, len(width))
		for i := range width {
			dash[i] = strings.Repeat("-", width[i])
		}
		line(dash)
		for _, r := range t.Rows {
			if r == nil {
				line(dash)
				continue
			}
			line(r)
		}
		for _, n := range t.Notes {
			fmt.Fprintln(w, n)
		}
		fmt.Fprintln(w)
		return nil
	}
}

func usd(v int64) string { return money.FormatUSD(v) }

func pct(n, d int64) string {
	if d == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", float64(n)*100/float64(d))
}

// FormatUSD is exported for CLI output.
func FormatUSD(v int64) string { return usd(v) }
