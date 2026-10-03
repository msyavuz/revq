package pipeline

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/msyavuz/revq/internal/github"
	"github.com/msyavuz/revq/internal/store"
)

var noiseNames = map[string]bool{
	"package-lock.json": true, "yarn.lock": true, "pnpm-lock.yaml": true, "bun.lock": true,
	"bun.lockb": true, "Cargo.lock": true, "go.sum": true, "poetry.lock": true, "uv.lock": true,
	"Pipfile.lock": true, "Gemfile.lock": true, "composer.lock": true, "flake.lock": true,
}

var noiseSuffixes = []string{
	".min.js", ".min.css", ".map", ".snap", ".pb.go", "_pb2.py", ".lock", ".svg", ".png",
	".jpg", ".jpeg", ".gif", ".ico", ".webp", ".woff", ".woff2", ".ttf", ".pdf", ".po", ".pot",
}

var noiseDirs = []string{"vendor/", "node_modules/", "dist/", "build/", "third_party/", "__snapshots__/"}

// isNoise reports files not worth model tokens: lockfiles, generated, vendored, binary.
func isNoise(p string, extra []string) bool {
	base := path.Base(p)
	if noiseNames[base] {
		return true
	}
	for _, s := range noiseSuffixes {
		if strings.HasSuffix(base, s) {
			return true
		}
	}
	for _, d := range noiseDirs {
		if strings.HasPrefix(p, d) || strings.Contains(p, "/"+d) {
			return true
		}
	}
	for _, g := range extra {
		if strings.HasSuffix(g, "/") {
			if strings.HasPrefix(p, g) || strings.Contains(p, "/"+g) {
				return true
			}
			continue
		}
		if ok, _ := path.Match(g, base); ok {
			return true
		}
		if ok, _ := path.Match(g, p); ok {
			return true
		}
	}
	return false
}

func parseGlobs(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// diffLine is one parsed line of a GitHub patch.
type diffLine struct {
	n    int    // new-file line number; 0 for removed lines and hunk headers
	kind string // add, del, ctx, hunk, meta
	text string // without the leading +, - or space
}

func parsePatch(patch string) []diffLine {
	var out []diffLine
	n := 0
	for _, line := range strings.Split(strings.TrimRight(patch, "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "@@"):
			// @@ -a,b +c,d @@
			if i := strings.Index(line, "+"); i >= 0 {
				rest := line[i+1:]
				end := strings.IndexAny(rest, ", ")
				if end < 0 {
					end = len(rest)
				}
				n, _ = strconv.Atoi(rest[:end])
			}
			out = append(out, diffLine{kind: "hunk", text: line})
		case strings.HasPrefix(line, "-"):
			out = append(out, diffLine{kind: "del", text: line[1:]})
		case strings.HasPrefix(line, "\\"):
			out = append(out, diffLine{kind: "meta", text: line})
		case strings.HasPrefix(line, "+"):
			out = append(out, diffLine{n: n, kind: "add", text: line[1:]})
			n++
		default:
			out = append(out, diffLine{n: n, kind: "ctx", text: strings.TrimPrefix(line, " ")})
			n++
		}
	}
	return out
}

// numbered prefixes every line present in the new file with its line number,
// so the model can cite lines without counting hunks. It also returns the set
// of lines GitHub will accept an inline comment on.
func numbered(patch string) (string, map[int]bool) {
	valid := map[int]bool{}
	var b strings.Builder
	for _, l := range parsePatch(patch) {
		switch l.kind {
		case "hunk", "meta":
			b.WriteString(l.text)
		case "del":
			b.WriteString("      -" + l.text)
		case "add":
			valid[l.n] = true
			fmt.Fprintf(&b, "%5d +%s", l.n, l.text)
		default:
			valid[l.n] = true
			fmt.Fprintf(&b, "%5d  %s", l.n, l.text)
		}
		b.WriteByte('\n')
	}
	return b.String(), valid
}

// snippet returns the diff lines around a new-file line, staying inside its
// hunk: a few lines of lead-in, the line itself, and one line after.
func snippet(patch string, line, before, after int) []store.SnippetLine {
	lines := parsePatch(patch)
	at := -1
	for i, l := range lines {
		if l.n == line && (l.kind == "add" || l.kind == "ctx") {
			at = i
			break
		}
	}
	if at < 0 {
		return nil
	}
	from, to := at, at
	for from > 0 && at-from < before && lines[from-1].kind != "hunk" {
		from--
	}
	for to < len(lines)-1 && to-at < after && lines[to+1].kind != "hunk" {
		to++
	}
	var out []store.SnippetLine
	for i := from; i <= to; i++ {
		l := lines[i]
		if l.kind == "meta" {
			continue
		}
		text := l.text
		if len(text) > 240 {
			text = text[:240] + "…"
		}
		out = append(out, store.SnippetLine{N: l.n, Kind: l.kind, Text: text, Target: i == at})
	}
	return out
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := strings.LastIndexByte(s[:max], '\n')
	if cut <= 0 {
		cut = max
	}
	return s[:cut] + "\n… [truncated]\n"
}

func fileHeader(f github.File) string {
	return fmt.Sprintf("### %s (%s +%d/-%d)\n", f.Path, f.Status, f.Additions, f.Deletions)
}

// fileList is the cheap overview of the whole PR: one line per file.
func fileList(files []github.File, noise map[string]bool, max int) string {
	var b strings.Builder
	for i, f := range files {
		if i == max {
			fmt.Fprintf(&b, "… and %d more files\n", len(files)-max)
			break
		}
		tag := ""
		if noise[f.Path] {
			tag = " [generated/lockfile, diff omitted]"
		} else if f.Patch == "" {
			tag = " [no diff available]"
		}
		fmt.Fprintf(&b, "%s %s +%d/-%d%s\n", f.Status, f.Path, f.Additions, f.Deletions, tag)
	}
	return b.String()
}

type reviewPlan struct {
	Chunks  []string
	Skipped []string
	Valid   map[string]map[int]bool
	Patch   map[string]string
}

// planReview packs numbered diffs into at most maxChunks prompts of chunkChars
// each, biggest changes first. Whatever doesn't fit is reported, not silently dropped.
func planReview(files []github.File, noise map[string]bool, chunkChars, maxChunks int) reviewPlan {
	plan := reviewPlan{Valid: map[string]map[int]bool{}, Patch: map[string]string{}}
	var real []github.File
	for _, f := range files {
		switch {
		case noise[f.Path]:
		case f.Patch == "":
			plan.Skipped = append(plan.Skipped, f.Path+" (no diff available)")
		default:
			real = append(real, f)
		}
	}
	sort.SliceStable(real, func(i, j int) bool {
		return real[i].Additions+real[i].Deletions > real[j].Additions+real[j].Deletions
	})
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			plan.Chunks = append(plan.Chunks, cur.String())
			cur.Reset()
		}
	}
	for _, f := range real {
		text, valid := numbered(f.Patch)
		part := fileHeader(f) + truncate(text, chunkChars-200) + "\n"
		if cur.Len()+len(part) > chunkChars {
			flush()
		}
		if len(plan.Chunks) >= maxChunks {
			plan.Skipped = append(plan.Skipped, f.Path)
			continue
		}
		plan.Valid[f.Path] = valid
		plan.Patch[f.Path] = f.Patch
		cur.WriteString(part)
	}
	flush()
	return plan
}
