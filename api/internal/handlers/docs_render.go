// Standalone HTML rendering for the docs viewer.
//
// Mirrors the in-app dashboard's markdown rules so /api/docs/{slug}.html
// shows the same content with the same typography — just as a real
// browser page that anyone can bookmark or share, no auth required.

package handlers

import (
	"fmt"
	"html"
	"regexp"
	"strings"
)

// renderHTMLPage wraps the rendered body in a full HTML5 document with
// dark/light theme tokens, Inter for body + JetBrains Mono for code,
// and the same editorial spacing the dashboard uses.
func renderHTMLPage(title, slug, src, baseURL string) string {
	body := renderMarkdownToHTML(src)
	safeTitle := html.EscapeString(title)
	return fmt.Sprintf(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s — PersonalS3 docs</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700&family=JetBrains+Mono:wght@400;500&display=swap" rel="stylesheet">
<style>
:root {
  --bg:        #fafaf9;
  --surface:   #ffffff;
  --border:    #e7e5e4;
  --border-soft:#f5f5f4;
  --text:      #0c0a09;
  --text-soft: #44403c;
  --muted:     #78716c;
  --link:      #1d4ed8;
  --code-bg:   #f5f5f4;
}
@media (prefers-color-scheme: dark) {
  :root {
    --bg:        #0c0a09;
    --surface:   #1c1917;
    --border:    #292524;
    --border-soft:#1c1917;
    --text:      #fafaf9;
    --text-soft: #d6d3d1;
    --muted:     #a8a29e;
    --link:      #93c5fd;
    --code-bg:   #292524;
  }
}

* { box-sizing: border-box; }
html, body { margin: 0; padding: 0; background: var(--bg); color: var(--text-soft); }
body {
  font-family: "Inter", system-ui, -apple-system, sans-serif;
  font-size: 17px;
  line-height: 1.75;
  -webkit-font-smoothing: antialiased;
  font-feature-settings: "cv11", "ss01", "ss03";
}

.page { max-width: 880px; margin: 0 auto; padding: 56px 32px 96px; }
@media (max-width: 640px) { .page { padding: 32px 20px 64px; } }

.topbar {
  display: flex; align-items: center; gap: 12px;
  padding-bottom: 24px; margin-bottom: 32px;
  border-bottom: 1px solid var(--border-soft);
  font-size: 13px;
}
.topbar .brand { font-weight: 600; color: var(--text); letter-spacing: -0.01em; }
.topbar .crumb { color: var(--muted); }
.topbar .crumb code { font-family: "JetBrains Mono", monospace; font-size: 12px; }

article { max-width: 72ch; }
article > * + * { margin-top: 1.25em; }

h1, h2, h3, h4 {
  color: var(--text); font-weight: 600;
  line-height: 1.2; letter-spacing: -0.02em;
  scroll-margin-top: 60px;
}
h1 { font-size: 2.5rem; margin: 0 0 0.6em; letter-spacing: -0.03em; font-weight: 700; }
h2 { font-size: 1.7rem; margin-top: 3rem; padding-bottom: 0.4rem; border-bottom: 1px solid var(--border-soft); }
h3 { font-size: 1.3rem; margin-top: 2.5rem; }
h4 { font-size: 1.1rem; margin-top: 2rem; }

p, li { color: var(--text-soft); }
strong { color: var(--text); font-weight: 600; }

a { color: var(--link); text-decoration: underline; text-decoration-thickness: 1px; text-underline-offset: 3px; }

code:not(pre code) {
  font-family: "JetBrains Mono", ui-monospace, monospace;
  font-size: 0.875em;
  background: var(--code-bg);
  color: var(--text);
  padding: 0.1em 0.4em;
  border-radius: 4px;
  border: 1px solid var(--border-soft);
}
pre {
  font-family: "JetBrains Mono", ui-monospace, monospace;
  background: var(--code-bg);
  color: var(--text);
  padding: 1rem 1.25rem;
  border-radius: 8px;
  border: 1px solid var(--border);
  overflow-x: auto;
  font-size: 0.875rem;
  line-height: 1.6;
}
pre code { background: transparent; border: 0; padding: 0; font-size: inherit; }

ul, ol { padding-left: 1.5rem; margin: 1em 0; }
li { margin: 0.4em 0; }
li::marker { color: var(--muted); }

table {
  width: 100%%; border-collapse: collapse; font-size: 0.9rem;
  margin: 1.5em 0;
}
th, td { text-align: left; padding: 0.6em 1em; border-bottom: 1px solid var(--border-soft); }
th { color: var(--text); font-weight: 600; border-bottom: 1px solid var(--border); }

blockquote {
  border-left: 3px solid var(--border);
  padding: 0.4em 0 0.4em 1.1em;
  margin: 1.5em 0;
  color: var(--muted);
  font-style: italic;
}

hr { border: 0; border-top: 1px solid var(--border-soft); margin: 3rem 0; }

img, video {
  max-width: 100%%; border-radius: 8px; border: 1px solid var(--border);
  margin: 1.5em auto; display: block;
}

footer {
  margin-top: 80px; padding-top: 24px;
  border-top: 1px solid var(--border-soft);
  color: var(--muted); font-size: 13px;
}
footer a { color: var(--text-soft); }
</style>
</head>
<body>
<div class="page">
  <div class="topbar">
    <a href="%s/dashboard/docs" class="brand" style="text-decoration:none">PersonalS3 docs</a>
    <span class="crumb">/ <code>%s</code></span>
  </div>
  <article>%s</article>
  <footer>
    Served from a public PersonalS3 bucket. ·
    <a href="%s/dashboard/docs">Open in dashboard</a>
  </footer>
</div>
</body>
</html>`, safeTitle, baseURL, html.EscapeString(slug), body, baseURL)
}

// ----------------------------------------------------------------------------
// renderMarkdownToHTML — same subset the dashboard's lib/markdown.ts
// supports: headings (with auto-anchors), paragraphs, bold/italic, inline
// + fenced code, lists, links, images, video, tables, blockquote, hr.
// Not a full CommonMark impl; matches what the docs actually use.

func renderMarkdownToHTML(src string) string {
	lines := strings.Split(src, "\n")
	var out []string
	var paraBuf []string
	var inFence bool
	var fenceLang string
	var fenceBuf []string
	var listStack []string

	flushPara := func() {
		if len(paraBuf) == 0 {
			return
		}
		out = append(out, "<p>"+renderInline(strings.Join(paraBuf, " "))+"</p>")
		paraBuf = nil
	}
	flushLists := func() {
		for len(listStack) > 0 {
			out = append(out, "</"+listStack[len(listStack)-1]+">")
			listStack = listStack[:len(listStack)-1]
		}
	}
	seenSlugs := map[string]int{}
	uniqueSlug := func(s string) string {
		n := seenSlugs[s]
		seenSlugs[s] = n + 1
		if n == 0 {
			return s
		}
		return fmt.Sprintf("%s-%d", s, n)
	}

	headingRe := regexp.MustCompile(`^(#{1,6})\s+(.+)$`)
	ulRe := regexp.MustCompile(`^\s*[-*]\s+(.+)$`)
	olRe := regexp.MustCompile(`^\s*\d+\.\s+(.+)$`)
	tableSepRe := regexp.MustCompile(`^\|[\s|:-]+\|$`)
	bqRe := regexp.MustCompile(`^>\s?(.*)$`)
	imgRe := regexp.MustCompile(`^!\[([^\]]*)\]\(([^)]+)\)\s*$`)
	vidRe := regexp.MustCompile(`^@video\[([^\]]*)\]\(([^)]+)\)\s*$`)
	fenceRe := regexp.MustCompile("^```(\\w*)\\s*$")

	for i := 0; i < len(lines); i++ {
		ln := strings.TrimRight(lines[i], " \t\r")

		if m := fenceRe.FindStringSubmatch(ln); m != nil {
			if inFence {
				out = append(out, fmt.Sprintf(`<pre><code class="language-%s">%s</code></pre>`,
					html.EscapeString(fenceLang), html.EscapeString(strings.Join(fenceBuf, "\n"))))
				inFence = false
				fenceBuf = nil
				fenceLang = ""
			} else {
				flushPara()
				flushLists()
				inFence = true
				fenceLang = m[1]
			}
			continue
		}
		if inFence {
			fenceBuf = append(fenceBuf, lines[i])
			continue
		}

		if ln == "" {
			flushPara()
			flushLists()
			continue
		}

		if m := headingRe.FindStringSubmatch(ln); m != nil {
			flushPara()
			flushLists()
			level := len(m[1])
			text := m[2]
			slug := uniqueSlug(slugifyText(text))
			anchor := ""
			if level > 1 {
				anchor = fmt.Sprintf(` <a href="#%s" class="anchor" aria-hidden="true" style="opacity:0.3;text-decoration:none">#</a>`, slug)
			}
			out = append(out, fmt.Sprintf(`<h%d id="%s">%s%s</h%d>`,
				level, slug, renderInline(text), anchor, level))
			continue
		}

		if regexp.MustCompile(`^---+$`).MatchString(ln) {
			flushPara()
			flushLists()
			out = append(out, "<hr />")
			continue
		}

		// Table
		if strings.HasPrefix(ln, "|") && i+1 < len(lines) && tableSepRe.MatchString(strings.TrimSpace(lines[i+1])) {
			flushPara()
			flushLists()
			header := splitTableRow(ln)
			i += 2 // skip separator
			var body [][]string
			for i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "|") {
				body = append(body, splitTableRow(lines[i]))
				i++
			}
			i-- // re-eval the line we stopped on
			var b strings.Builder
			b.WriteString("<table><thead><tr>")
			for _, c := range header {
				b.WriteString("<th>" + renderInline(c) + "</th>")
			}
			b.WriteString("</tr></thead><tbody>")
			for _, row := range body {
				b.WriteString("<tr>")
				for _, c := range row {
					b.WriteString("<td>" + renderInline(c) + "</td>")
				}
				b.WriteString("</tr>")
			}
			b.WriteString("</tbody></table>")
			out = append(out, b.String())
			continue
		}

		if m := imgRe.FindStringSubmatch(ln); m != nil {
			flushPara()
			flushLists()
			alt := html.EscapeString(m[1])
			out = append(out, fmt.Sprintf(`<figure><img src="%s" alt="%s" loading="lazy" />%s</figure>`,
				html.EscapeString(m[2]), alt,
				caption(m[1])))
			continue
		}
		if m := vidRe.FindStringSubmatch(ln); m != nil {
			flushPara()
			flushLists()
			out = append(out, fmt.Sprintf(`<figure><video src="%s" controls preload="metadata"></video>%s</figure>`,
				html.EscapeString(m[2]), caption(m[1])))
			continue
		}

		if m := ulRe.FindStringSubmatch(ln); m != nil {
			flushPara()
			if len(listStack) == 0 || listStack[len(listStack)-1] != "ul" {
				flushLists()
				listStack = append(listStack, "ul")
				out = append(out, "<ul>")
			}
			out = append(out, "<li>"+renderInline(m[1])+"</li>")
			continue
		}
		if m := olRe.FindStringSubmatch(ln); m != nil {
			flushPara()
			if len(listStack) == 0 || listStack[len(listStack)-1] != "ol" {
				flushLists()
				listStack = append(listStack, "ol")
				out = append(out, "<ol>")
			}
			out = append(out, "<li>"+renderInline(m[1])+"</li>")
			continue
		}

		if m := bqRe.FindStringSubmatch(ln); m != nil {
			flushPara()
			flushLists()
			out = append(out, "<blockquote><p>"+renderInline(m[1])+"</p></blockquote>")
			continue
		}

		flushLists()
		paraBuf = append(paraBuf, ln)
	}
	flushPara()
	flushLists()
	if inFence {
		out = append(out, "<pre><code>"+html.EscapeString(strings.Join(fenceBuf, "\n"))+"</code></pre>")
	}
	return strings.Join(out, "\n")
}

// Inline-level transforms. Escape first, then re-introduce markup
// tokens — links / images before code so a URL in a code span doesn't
// accidentally re-render.
func renderInline(s string) string {
	t := html.EscapeString(s)

	// Inline image (escaped) — must come before link to disambiguate ![...]
	imgRe := regexp.MustCompile(`!\[([^\]]*)\]\(([^)]+)\)`)
	t = imgRe.ReplaceAllString(t, `<img src="$2" alt="$1" loading="lazy" />`)
	// Link
	linkRe := regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	t = linkRe.ReplaceAllString(t, `<a href="$2" target="_blank" rel="noreferrer">$1</a>`)
	// Inline code
	t = regexp.MustCompile("`([^`]+)`").ReplaceAllString(t, "<code>$1</code>")
	// Bold
	t = regexp.MustCompile(`\*\*([^*]+)\*\*`).ReplaceAllString(t, "<strong>$1</strong>")
	t = regexp.MustCompile(`__([^_]+)__`).ReplaceAllString(t, "<strong>$1</strong>")
	// Italic — guard against the leading * we just used for bold
	t = regexp.MustCompile(`(^|[^*])\*([^*\n]+)\*`).ReplaceAllString(t, "$1<em>$2</em>")
	return t
}

func splitTableRow(line string) []string {
	parts := strings.Split(line, "|")
	if len(parts) < 2 {
		return nil
	}
	parts = parts[1 : len(parts)-1]
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

var slugBadChars = regexp.MustCompile(`[^\w\s-]`)
var slugSpaces = regexp.MustCompile(`\s+`)

func slugifyText(s string) string {
	s = strings.ToLower(s)
	s = slugBadChars.ReplaceAllString(s, "")
	s = strings.TrimSpace(s)
	s = slugSpaces.ReplaceAllString(s, "-")
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}

func caption(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return "<figcaption>" + renderInline(s) + "</figcaption>"
}
