package sqlq

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// SavedQuery is a named .sql file from a queries directory. The leading "--" comment block is
// its documentation: the first line is the description, "-- param: name ..." lines document
// parameters.
type SavedQuery struct {
	Name, Path, Description string
	Params                  []string
	SQL                     string
}

var queryName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func parseSaved(name, path string) (*SavedQuery, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	q := &SavedQuery{Name: name, Path: path, SQL: string(b)}
	for _, l := range strings.Split(q.SQL, "\n") {
		l = strings.TrimSpace(l)
		if !strings.HasPrefix(l, "--") {
			if l == "" {
				continue
			}
			break
		}
		t := strings.TrimSpace(strings.TrimPrefix(l, "--"))
		switch {
		case strings.HasPrefix(t, "param:"):
			q.Params = append(q.Params, strings.TrimSpace(strings.TrimPrefix(t, "param:")))
		case q.Description == "" && t != "":
			q.Description = t
		}
	}
	return q, nil
}

// FindQuery looks up <name>.sql in dirs (first match wins).
func FindQuery(name string, dirs []string) (*SavedQuery, error) {
	if !queryName.MatchString(name) {
		return nil, fmt.Errorf("bad query name %q", name)
	}
	for _, d := range dirs {
		p := filepath.Join(d, name+".sql")
		if _, err := os.Stat(p); err == nil {
			return parseSaved(name, p)
		}
	}
	return nil, fmt.Errorf("no saved query %q in %s (list them with `finance sql --queries`)", name, strings.Join(dirs, ", "))
}

// ListQueries returns every saved query across dirs (earlier dirs shadow later ones).
func ListQueries(dirs []string) ([]*SavedQuery, error) {
	seen := map[string]bool{}
	var out []*SavedQuery
	for _, d := range dirs {
		files, _ := filepath.Glob(filepath.Join(d, "*.sql"))
		sort.Strings(files)
		for _, f := range files {
			name := strings.TrimSuffix(filepath.Base(f), ".sql")
			if seen[name] {
				continue
			}
			seen[name] = true
			q, err := parseSaved(name, f)
			if err != nil {
				return nil, err
			}
			out = append(out, q)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
