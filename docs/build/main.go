// Command build turns docs/guide.md into the standalone documentation site in
// docs/_site. Run it from the repository root:
//
//	go run ./docs/build
package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
)

//go:embed page.html
var pageTemplate string

//go:embed site.css
var siteCSS []byte

const (
	source = "docs/guide.md"
	outDir = "docs/_site"
	static = "internal/web/static"
)

type section struct{ ID, Title string }

var (
	h1Re = regexp.MustCompile(`(?s)<h1[^>]*>(.*?)</h1>`)
	h2Re = regexp.MustCompile(`(?s)<h2 id="([^"]+)">(.*?)</h2>`)
	tags = regexp.MustCompile(`<[^>]+>`)
)

func main() {
	if err := build(); err != nil {
		fmt.Fprintln(os.Stderr, "docs build:", err)
		os.Exit(1)
	}
	fmt.Println("wrote", filepath.Join(outDir, "index.html"))
}

func build() error {
	md, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("%w (run from the repository root)", err)
	}
	r := goldmark.New(goldmark.WithExtensions(extension.GFM), goldmark.WithParserOptions(parser.WithAutoHeadingID()))
	var body bytes.Buffer
	if err := r.Convert(md, &body); err != nil {
		return err
	}
	html := body.String()

	// The page template sets the title itself, so lift the h1 out of the body.
	title := "revq guide"
	if m := h1Re.FindStringSubmatch(html); m != nil {
		title = tags.ReplaceAllString(m[1], "")
		html = strings.Replace(html, m[0], "", 1)
	}
	var sections []section
	for _, m := range h2Re.FindAllStringSubmatch(html, -1) {
		sections = append(sections, section{ID: m[1], Title: tags.ReplaceAllString(m[2], "")})
	}

	if err := os.RemoveAll(outDir); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(outDir, "fonts"), 0o755); err != nil {
		return err
	}
	// The site shares the app's typefaces and icon so the two look related.
	for _, f := range []string{"icon.svg", "fonts/schibsted-grotesk.woff2", "fonts/commit-mono.woff2",
		"fonts/OFL-schibsted-grotesk.txt", "fonts/OFL-commit-mono.txt"} {
		if err := copyFile(filepath.Join(static, f), filepath.Join(outDir, f)); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(outDir, "site.css"), siteCSS, 0o644); err != nil {
		return err
	}

	out, err := os.Create(filepath.Join(outDir, "index.html"))
	if err != nil {
		return err
	}
	defer out.Close()
	return template.Must(template.New("page").Parse(pageTemplate)).Execute(out, map[string]any{
		"Title":    title,
		"Sections": sections,
		"Body":     template.HTML(html), // our own guide, rendered by goldmark
	})
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
