package mcpserver

import (
	"fmt"
	"io/fs"
	"sort"
	"strings"

	platformdocs "ebof-wg-mesh/docs"
)

// docIndex is a tiny keyword index over the embedded platform docs,
// sectioned by markdown heading for precise snippets.
type docIndex struct {
	files    map[string]string // name -> full text
	names    []string
	sections []docSection
}

type docSection struct {
	file    string
	heading string
	text    string
}

func loadDocs() (*docIndex, error) {
	idx := &docIndex{files: map[string]string{}}
	entries, err := fs.ReadDir(platformdocs.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("load embedded docs: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		data, err := platformdocs.FS.ReadFile(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("load embedded docs: %w", err)
		}
		idx.files[entry.Name()] = string(data)
		idx.names = append(idx.names, entry.Name())
		idx.sections = append(idx.sections, splitSections(entry.Name(), string(data))...)
	}
	sort.Strings(idx.names)
	return idx, nil
}

func splitSections(file, text string) []docSection {
	var sections []docSection
	current := docSection{file: file, heading: file}
	var body strings.Builder
	flush := func() {
		current.text = strings.TrimSpace(body.String())
		if current.text != "" {
			sections = append(sections, current)
		}
		body.Reset()
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "# ") || strings.HasPrefix(line, "## ") {
			flush()
			current = docSection{file: file, heading: strings.TrimSpace(strings.TrimLeft(line, "# "))}
		}
		body.WriteString(line)
		body.WriteByte('\n')
	}
	flush()
	return sections
}

// search ranks sections by query-term frequency (heading hits weigh more).
func (idx *docIndex) search(query string, limit int) []map[string]any {
	terms := tokenize(query)
	if len(terms) == 0 || limit <= 0 {
		return nil
	}
	type scored struct {
		section docSection
		score   int
	}
	var hits []scored
	for _, section := range idx.sections {
		lowerText := strings.ToLower(section.text)
		lowerHeading := strings.ToLower(section.heading)
		score := 0
		for _, term := range terms {
			score += strings.Count(lowerText, term)
			score += 5 * strings.Count(lowerHeading, term)
		}
		if score > 0 {
			hits = append(hits, scored{section, score})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	out := make([]map[string]any, 0, min(limit, len(hits)))
	for _, hit := range hits[:min(limit, len(hits))] {
		out = append(out, map[string]any{
			"file":    hit.section.file,
			"heading": hit.section.heading,
			"score":   hit.score,
			"snippet": snippet(hit.section.text, terms),
		})
	}
	return out
}

func tokenize(query string) []string {
	fields := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_'
	})
	var terms []string
	for _, field := range fields {
		if len(field) >= 2 {
			terms = append(terms, field)
		}
	}
	return terms
}

// snippet returns a window around the first term hit.
func snippet(text string, terms []string) string {
	const window = 600
	lower := strings.ToLower(text)
	at := -1
	for _, term := range terms {
		if idx := strings.Index(lower, term); idx >= 0 && (at < 0 || idx < at) {
			at = idx
		}
	}
	if at < 0 {
		at = 0
	}
	start := max(0, at-window/3)
	end := min(len(text), start+window)
	out := text[start:end]
	if start > 0 {
		out = "…" + out
	}
	if end < len(text) {
		out += "…"
	}
	return out
}

func (idx *docIndex) fetch(name string) (string, bool) {
	name = strings.TrimPrefix(name, "docs/")
	if !strings.HasSuffix(name, ".md") {
		name += ".md"
	}
	text, ok := idx.files[name]
	return text, ok
}
