package server

import (
	"html"
	"net/http"
	"regexp"
	"strings"
)

// handleDocs serves the onboarding guide as a human-readable HTML page at /docs.
// It renders the same canonical agentGuide that /api/guide and /llms.txt expose
// as raw markdown, so there is a single source of truth. Kept dependency-free
// and self-contained (no CDN) to honor the hub's offline-first, minimal-module
// design — the renderer below covers exactly the markdown the guide uses.
func (s *Server) handleDocs(w http.ResponseWriter, r *http.Request) {
	base := s.baseURL(r)
	md := strings.ReplaceAll(agentGuide, "{BASE}", base)
	page := docsPageTop + renderMarkdownToHTML(md) + docsPageBottom
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(page))
}

var (
	mdCodeSpan = regexp.MustCompile("`([^`]+)`")
	mdBold     = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	mdLink     = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	mdHeading  = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
)

// renderInline escapes HTML, then applies inline markdown (code, bold, links).
func renderInline(s string) string {
	s = html.EscapeString(s)
	s = mdCodeSpan.ReplaceAllString(s, "<code>$1</code>")
	s = mdBold.ReplaceAllString(s, "<strong>$1</strong>")
	s = mdLink.ReplaceAllString(s, `<a href="$2">$1</a>`)
	return s
}

// renderMarkdownToHTML converts the guide's markdown subset to HTML: headings,
// horizontal rules, fenced code blocks, unordered lists, pipe tables, and
// paragraphs with inline formatting.
func renderMarkdownToHTML(md string) string {
	lines := strings.Split(md, "\n")
	var b strings.Builder

	flushPara := func(buf *[]string) {
		if len(*buf) == 0 {
			return
		}
		b.WriteString("<p>" + renderInline(strings.Join(*buf, " ")) + "</p>\n")
		*buf = (*buf)[:0]
	}

	var para []string
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		// Fenced code block
		if strings.HasPrefix(trimmed, "```") {
			flushPara(&para)
			var code []string
			for i++; i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```"); i++ {
				code = append(code, lines[i])
			}
			b.WriteString("<pre><code>" + html.EscapeString(strings.Join(code, "\n")) + "</code></pre>\n")
			continue
		}

		// Horizontal rule
		if trimmed == "---" {
			flushPara(&para)
			b.WriteString("<hr>\n")
			continue
		}

		// Heading
		if m := mdHeading.FindStringSubmatch(line); m != nil {
			flushPara(&para)
			level := len(m[1])
			tag := "h" + string(rune('0'+level))
			b.WriteString("<" + tag + ">" + renderInline(m[2]) + "</" + tag + ">\n")
			continue
		}

		// Pipe table: a header row followed by a |---| separator row
		if strings.HasPrefix(trimmed, "|") && i+1 < len(lines) &&
			strings.Contains(lines[i+1], "-") && strings.HasPrefix(strings.TrimSpace(lines[i+1]), "|") {
			flushPara(&para)
			header := splitTableRow(trimmed)
			b.WriteString("<table>\n<thead><tr>")
			for _, c := range header {
				b.WriteString("<th>" + renderInline(c) + "</th>")
			}
			b.WriteString("</tr></thead>\n<tbody>\n")
			i++ // skip separator row
			for i+1 < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i+1]), "|") {
				i++
				b.WriteString("<tr>")
				for _, c := range splitTableRow(strings.TrimSpace(lines[i])) {
					b.WriteString("<td>" + renderInline(c) + "</td>")
				}
				b.WriteString("</tr>\n")
			}
			b.WriteString("</tbody></table>\n")
			continue
		}

		// Unordered list
		if strings.HasPrefix(trimmed, "- ") {
			flushPara(&para)
			b.WriteString("<ul>\n")
			for i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "- ") {
				item := strings.TrimSpace(lines[i])[2:]
				b.WriteString("<li>" + renderInline(item) + "</li>\n")
				i++
			}
			i-- // step back; outer loop will advance
			b.WriteString("</ul>\n")
			continue
		}

		// Blank line ends a paragraph
		if trimmed == "" {
			flushPara(&para)
			continue
		}

		para = append(para, trimmed)
	}
	flushPara(&para)
	return b.String()
}

// splitTableRow splits a "| a | b | c |" row into trimmed cell strings.
func splitTableRow(row string) []string {
	row = strings.Trim(row, "|")
	parts := strings.Split(row, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

const docsPageTop = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>AgentHub — docs</title>
<style>
  * { margin: 0; padding: 0; box-sizing: border-box; }
  body { font-family: 'SF Mono','Menlo','Consolas',monospace;
    background: #0a0a0a; color: #e0e0e0; font-size: 14px; line-height: 1.65; }
  .wrap { max-width: 820px; margin: 0 auto; padding: 48px 24px 96px; }
  a { color: #7aa2f7; text-decoration: none; }
  a:hover { text-decoration: underline; }
  nav { margin-bottom: 32px; font-size: 12px; color: #555; }
  nav a { color: #7aa2f7; }
  h1 { font-size: 24px; color: #fff; margin: 8px 0 16px; letter-spacing: 0.5px; }
  h2 { font-size: 17px; color: #fff; margin: 36px 0 12px;
    padding-bottom: 6px; border-bottom: 1px solid #1a1a1a; }
  h3 { font-size: 14px; color: #cbd5f5; margin: 24px 0 8px; }
  p { margin: 12px 0; color: #cfcfcf; }
  ul { margin: 12px 0 12px 24px; }
  li { margin: 4px 0; color: #cfcfcf; }
  hr { border: none; border-top: 1px solid #1a1a1a; margin: 28px 0; }
  code { background: #161616; color: #f0c674; padding: 1px 5px;
    border-radius: 4px; font-size: 13px; }
  pre { background: #0f0f0f; border: 1px solid #1a1a1a; border-radius: 8px;
    padding: 16px; overflow-x: auto; margin: 16px 0; }
  pre code { background: none; color: #9ece6a; padding: 0; }
  strong { color: #fff; font-weight: 600; }
  table { border-collapse: collapse; width: 100%; margin: 16px 0; font-size: 13px; }
  th, td { border: 1px solid #1a1a1a; padding: 7px 10px; text-align: left; }
  th { background: #111; color: #aaa; font-weight: 600; }
  td { color: #cfcfcf; }
  td code { color: #f0c674; }
</style>
</head>
<body>
<div class="wrap">
<nav>&larr; <a href="/">dashboard</a> &nbsp;·&nbsp; raw: <a href="/api/guide">/api/guide</a> &nbsp;·&nbsp; <a href="/llms.txt">/llms.txt</a></nav>
`

const docsPageBottom = `</div>
</body>
</html>
`
