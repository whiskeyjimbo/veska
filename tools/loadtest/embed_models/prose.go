//go:build eval

// Prose corpus loader for the embed-models bench (solov2-0k5h.4).
// Walks .md files under a corpus root, splits each into sections at
// H1/H2 headings, and emits one document per section. Section docs use
// a "<relative-path>#<heading-slug>" name so hand-curated prose.jsonl
// can address them stably.
//
// We use a minimal ATX-only line scanner — no inline markdown parsing
// needed since we treat the section body as a flat text blob to embed.

package embed_models

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

)

// embedProseCorpus walks .md files under root, splits each into
// sections, and embeds each section as one doc. Capped at maxDocs.
func embedProseCorpus(p Embedder, root string, maxDocs int) ([]doc, embedStats) {
	var docs []doc
	start := time.Now()
	var totalEmbedNS int64
	var nEmbeds int

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == "vendor" || name == ".git" || name == "node_modules" || name == "out" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".md" {
			return nil
		}
		if len(docs) >= maxDocs {
			return filepath.SkipAll
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if rel == "" {
			rel = path
		}
		for _, sec := range splitSections(string(src)) {
			if len(docs) >= maxDocs {
				return filepath.SkipAll
			}
			// Section text = heading + body; if there's no body just
			// embedding the title alone yields a meaningless vector.
			if strings.TrimSpace(sec.body) == "" {
				continue
			}
			name := rel + "#" + slugify(sec.heading)
			text := sec.heading + "\n" + sec.body
			t0 := time.Now()
			v, err := p.Embed(context.Background(), text)
			totalEmbedNS += time.Since(t0).Nanoseconds()
			nEmbeds++
			if err != nil {
				continue
			}
			docs = append(docs, doc{name: name, file: path, vec: v})
		}
		return nil
	})
	_ = walkErr // walk errors are non-fatal — partial corpora are fine
	stats := embedStats{total: time.Since(start)}
	if nEmbeds > 0 {
		stats.avgMS = float64(totalEmbedNS) / float64(nEmbeds) / 1e6
	}
	return docs, stats
}

// section is one markdown H1/H2-delimited region.
type section struct {
	heading string
	body    string
}

// splitSections cuts src on H1 (#) and H2 (##) ATX headings. Anything
// before the first heading becomes a section whose heading is "" (the
// recall metric just won't ever match that one). H3+ headings remain
// inside their parent section.
func splitSections(src string) []section {
	lines := strings.Split(src, "\n")
	var out []section
	var cur section
	flush := func() {
		if cur.heading != "" || strings.TrimSpace(cur.body) != "" {
			out = append(out, cur)
		}
	}
	for _, line := range lines {
		// ATX H1 / H2 only. Tighter than the spec but matches the
		// docs/ structure we want to index.
		if strings.HasPrefix(line, "# ") || strings.HasPrefix(line, "## ") {
			flush()
			heading := strings.TrimSpace(strings.TrimLeft(line, "#"))
			cur = section{heading: heading}
			continue
		}
		if cur.body != "" {
			cur.body += "\n"
		}
		cur.body += line
	}
	flush()
	return out
}

// slugify normalises a heading into a kebab-case identifier for the
// doc-name key. Matches the convention used by most static-site
// generators so headline pairs are intuitive to author.
func slugify(s string) string {
	var b strings.Builder
	prevDash := true
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case !prevDash:
			b.WriteByte('-')
			prevDash = true
		}
	}
	out := b.String()
	return strings.Trim(out, "-")
}
