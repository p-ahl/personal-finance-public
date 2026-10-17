package report

import (
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/p-ahl/personal-finance/internal/money"
)

// ProjectBudget is the optional data-side budget file (budgets.yaml; real values live only
// in the local data config). Example with placeholder figures:
//
//	projects:
//	  <code>:
//	    name: ...
//	    target_low: "100000.00"
//	    target_high: "150000.00"
//	    lines:
//	      - {cost: design, committed: "1000.00", budget: "", note: "fixed-fee contract"}
type ProjectBudget struct {
	Projects map[string]struct {
		Name       string `yaml:"name"`
		TargetLow  string `yaml:"target_low"`
		TargetHigh string `yaml:"target_high"`
		Note       string `yaml:"note"`
		Lines      []struct {
			Cost      string `yaml:"cost"`
			Committed string `yaml:"committed"`
			Budget    string `yaml:"budget"`
			Note      string `yaml:"note"`
		} `yaml:"lines"`
	} `yaml:"projects"`
}

// LoadProjectBudget reads a budgets file; a missing file is not an error.
func LoadProjectBudget(path string) (*ProjectBudget, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &ProjectBudget{}, nil
	}
	if err != nil {
		return nil, err
	}
	var pb ProjectBudget
	if err := yaml.Unmarshal(b, &pb); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &pb, nil
}

func parseUSD(s string) int64 {
	if s == "" {
		return 0
	}
	v, err := money.ParseMinor(s, 2)
	if err != nil {
		return 0
	}
	return v
}

type projLine struct {
	date, cost, acct, treat, cp, desc, docs, status string
	amt, open                                       int64
}

// Project reports cost to date for one project dimension (project:<code>): every capitalized
// (Assets:RealEstate*) or expensed line tagged with it, by cost code (dimension cost:<code>, or the
// last account segment), with paid vs open payable, co-owner shares, evidence, and budget/forecast.
func Project(db *sql.DB, code, asOf string, pb *ProjectBudget) ([]*Table, error) {
	rows, err := db.Query(`
		SELECT e.id, e.entry_date, a.name, a.account_type, l.amount,
		  COALESCE((SELECT d2.code FROM line_dimensions ld2 JOIN dimensions d2 ON d2.id = ld2.dimension_id
		            WHERE ld2.line_id = l.id AND d2.kind = 'cost' LIMIT 1), ''),
		  COALESCE(lc.name, ec.name, ''), COALESCE(NULLIF(l.memo,''), e.description),
		  COALESCE((SELECT group_concat(doc.doc_key, ' ') FROM entry_documents ed JOIN documents doc ON doc.id = ed.document_id
		            WHERE ed.entry_id = e.id), ''),
		  COALESCE((SELECT ac.id FROM accrual_links al JOIN accruals ac ON ac.id = al.accrual_id
		            WHERE al.entry_id = e.id AND al.role IN ('accrual','journal') LIMIT 1), 0),
		  e.source
		FROM journal_lines l
		JOIN journal_entries e ON e.id = l.entry_id AND e.status = 'posted' AND e.entry_date <= ?
		JOIN accounts a ON a.id = l.account_id
		JOIN line_dimensions ld ON ld.line_id = l.id
		JOIN dimensions d ON d.id = ld.dimension_id AND d.kind = 'project' AND d.code = ?
		LEFT JOIN counterparties lc ON lc.id = l.counterparty_id
		LEFT JOIN counterparties ec ON ec.id = e.counterparty_id
		WHERE a.account_type = 'expense' OR (a.account_type = 'asset' AND a.level2 = 'RealEstate')
		ORDER BY e.entry_date, e.id, l.line_no`, asOf, code)
	if err != nil {
		return nil, err
	}
	var lines []projLine
	accIDs := map[int64]bool{}
	type raw struct {
		l     projLine
		accID int64
	}
	var raws []raw
	for rows.Next() {
		var eid, accID int64
		var l projLine
		var typ, src string
		if err := rows.Scan(&eid, &l.date, &l.acct, &typ, &l.amt, &l.cost, &l.cp, &l.desc, &l.docs, &accID, &src); err != nil {
			rows.Close()
			return nil, err
		}
		if l.cost == "" {
			segs := strings.Split(l.acct, ":")
			l.cost = strings.ToLower(segs[len(segs)-1])
		}
		l.treat = "capitalized"
		if typ == "expense" {
			l.treat = "expensed"
		}
		if l.docs == "" && src == "connector" {
			l.docs = "bank txn"
		}
		raws = append(raws, raw{l, accID})
		if accID != 0 {
			accIDs[accID] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// accrual status: recognized vs open on the balance account as of the date
	type accSt struct {
		key, balType, balName string
		recog, open           int64
	}
	st := map[int64]accSt{}
	for id := range accIDs {
		var s accSt
		err := db.QueryRow(`
			SELECT ac.accrual_key, COALESCE(ba.account_type,''), COALESCE(ba.name,''),
			  COALESCE(SUM(CASE WHEN al.role IN ('accrual','opening','journal') THEN l.amount ELSE 0 END),0),
			  COALESCE(SUM(l.amount),0)
			FROM accruals ac LEFT JOIN accounts ba ON ba.id = ac.balance_account_id
			LEFT JOIN accrual_links al ON al.accrual_id = ac.id
			LEFT JOIN journal_entries e ON e.id = al.entry_id AND e.status = 'posted' AND e.entry_date <= ?
			LEFT JOIN journal_lines l ON l.entry_id = e.id AND l.account_id = ac.balance_account_id
			WHERE ac.id = ? GROUP BY ac.id`, asOf, id).Scan(&s.key, &s.balType, &s.balName, &s.recog, &s.open)
		if err != nil {
			return nil, err
		}
		st[id] = s
	}
	for _, r := range raws {
		l := r.l
		l.status = "paid"
		if s, ok := st[r.accID]; ok {
			switch {
			case s.balType == "asset" && strings.Contains(s.balName, ":Receivable"):
				l.status = "co-owner/other share"
				if s.open != 0 {
					l.status += " (owed to us)"
				}
			case s.balType == "asset":
				l.status = "prepaid"
			case s.open != 0 && s.recog != 0:
				l.open = l.amt * s.open / s.recog // proportional share of what is still open
				l.status = "payable open " + usd(l.open)
				if l.open == l.amt {
					l.status = "payable (unpaid)"
				}
			}
			if l.docs == "" {
				l.docs = s.key
			}
		}
		lines = append(lines, l)
	}

	// summary by cost code
	type agg struct {
		cost, open, committed, budget int64
		treat, note                   string
	}
	by := map[string]*agg{}
	get := func(c string) *agg {
		if by[c] == nil {
			by[c] = &agg{}
		}
		return by[c]
	}
	for _, l := range lines {
		a := get(l.cost)
		a.cost += l.amt
		a.open += l.open
		if a.treat == "" || a.treat == l.treat {
			a.treat = l.treat
		} else {
			a.treat = "mixed"
		}
	}
	var title, foot []string
	if pb != nil {
		if p, ok := pb.Projects[code]; ok {
			if p.Name != "" {
				title = append(title, p.Name)
			}
			for _, bl := range p.Lines {
				a := get(bl.Cost)
				a.committed += parseUSD(bl.Committed)
				a.budget += parseUSD(bl.Budget)
				if bl.Note != "" {
					a.note = bl.Note
				}
			}
			if p.TargetLow != "" || p.TargetHigh != "" {
				foot = append(foot, fmt.Sprintf("Household all-in target: %s to %s.", usd(parseUSD(p.TargetLow)), usd(parseUSD(p.TargetHigh))))
			}
			if p.Note != "" {
				foot = append(foot, p.Note)
			}
		}
	}
	codes := make([]string, 0, len(by))
	for c := range by {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	sum := &Table{Title: fmt.Sprintf("Project %s: cost to date by cost code as of %s %s", code, asOf, strings.Join(title, " ")),
		Headers: []string{"Cost code", "Treatment", "Cost to date (our share)", "Of which unpaid", "Committed", "Budget", "Forecast", "Remaining to forecast", "Note"},
		Right:   []bool{false, false, true, true, true, true, true, true, false}}
	var tc, to, tcm, tb, tf int64
	for _, c := range codes {
		a := by[c]
		f := max(a.cost, a.committed, a.budget)
		treat := a.treat
		if treat == "" {
			treat = "-"
		}
		sum.Add(c, treat, usd(a.cost), usd(a.open), usd(a.committed), usd(a.budget), usd(f), usd(f-a.cost), a.note)
		tc, to, tcm, tb, tf = tc+a.cost, to+a.open, tcm+a.committed, tb+a.budget, tf+f
	}
	sum.Sep()
	sum.Add("Total", "", usd(tc), usd(to), usd(tcm), usd(tb), usd(tf), usd(tf-tc), "")
	sum.Notes = append([]string{
		"Forecast = max(cost to date, committed contracts, budget) per cost code; uncommitted, unbudgeted codes forecast at cost to date.",
		"Co-owner shares are credited against the cost code (our share only); the receivable sits in Assets:Receivable.",
	}, foot...)

	det := &Table{Title: fmt.Sprintf("Project %s: detail", code),
		Headers: []string{"Date", "Cost code", "Account", "Counterparty", "Description", "Amount", "Status", "Evidence"},
		Right:   []bool{false, false, false, false, false, true, false, false}}
	for _, l := range lines {
		d := l.desc
		if len(d) > 70 {
			d = d[:67] + "..."
		}
		det.Add(l.date, l.cost, l.acct, l.cp, d, usd(l.amt), l.status, l.docs)
	}
	return []*Table{sum, det}, nil
}
