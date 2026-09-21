package review

import (
	"regexp"
	"strings"
)

// headerRe matches an item header line: "### R6-01: title".
var headerRe = regexp.MustCompile(`^### (R\d+-\d+): (.+)$`)

// metaRe matches one metadata line under an item header.
var metaRe = regexp.MustCompile(`^- (file|severity|status): (.*)$`)

// responseRe matches the first line of an implementer response.
var responseRe = regexp.MustCompile(`^- response:\s*(.*)$`)

// Item is one review finding with its responses.
type Item struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	File      string   `json:"file,omitempty"`
	Severity  string   `json:"severity,omitempty"`
	Status    string   `json:"status,omitempty"`
	Body      string   `json:"body"`
	Responses []string `json:"responses"`
}

// Where the parser is within an item. modeSeek is both "before the
// first header" and "after a line that ended a response", where
// everything is ignored until the next terminator.
const (
	modeSeek = iota
	modeMeta
	modeBody
	modeResponse
)

// parser accumulates one item at a time as Parse walks the file.
type parser struct {
	items []Item
	cur   *Item
	body  []string
	resp  []string
	mode  int
	// sawMeta reports whether the item in progress has had at least
	// one metadata line, which decides what a blank line means.
	sawMeta bool
}

// Parse extracts items in document order. CRLF input is read as LF.
// An item starts at a "### " header outside a fenced block, takes the
// file, severity, and status lines that follow it (real reviewers
// leave a blank line in between), then a body that runs to the first
// response, the next header, a "---" line, or EOF. A response runs
// through indented continuation lines; a line that ends one without
// being a terminator is ignored, as is everything up to the next
// terminator. A file with no items returns an empty, non-nil slice.
// Responses is always non-nil.
func Parse(text string) []Item {
	p := parser{items: []Item{}}
	// Every carriage return goes before any structural matching: a
	// CRLF file would otherwise leave "---\r", which is not the
	// terminator, and trail a "\r" on every title and body line.
	text = strings.ReplaceAll(text, "\r", "")
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")

	var fenced bool
	for i, line := range lines {
		var next string
		if i+1 < len(lines) {
			next = lines[i+1]
		}

		// A fence marker, and everything inside a fence, is content:
		// never a header, a terminator, or a response.
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
			p.content(line, next)

			continue
		}
		if fenced {
			p.content(line, next)

			continue
		}

		p.structure(line, next)
	}
	p.flush()

	return p.items
}

// structure handles one line outside a fence, where headers,
// metadata, terminators, and response openers are recognised.
func (p *parser) structure(line, next string) {
	if m := headerRe.FindStringSubmatch(line); m != nil {
		p.startItem(m[1], m[2])

		return
	}
	if p.mode == modeMeta {
		if m := metaRe.FindStringSubmatch(line); m != nil {
			p.meta(m[1], m[2])

			return
		}
	}
	if line == "---" {
		p.flush()

		return
	}
	if m := responseRe.FindStringSubmatch(line); m != nil &&
		p.cur != nil {
		p.startResponse(m[1])

		return
	}

	p.content(line, next)
}

// content accumulates a line that is not a header, a terminator, or a
// response opener. next is the following line, which decides whether
// a blank line continues a response.
func (p *parser) content(line, next string) {
	switch p.mode {
	case modeMeta:
		// Blank lines between the header and the metadata are
		// skipped. Once a metadata line has been read the block is
		// contiguous, so the next blank line closes it and separates
		// it from the body.
		if strings.TrimSpace(line) == "" {
			if p.sawMeta {
				p.mode = modeBody
			}

			return
		}

		p.mode = modeBody
		p.body = append(p.body, line)
	case modeBody:
		p.body = append(p.body, line)
	case modeResponse:
		blank := strings.TrimSpace(line) == ""
		switch {
		case strings.HasPrefix(line, "  "):
			if !blank {
				p.resp = append(p.resp, strings.TrimSpace(line))
			}
		case blank && strings.HasPrefix(next, "  "):
		default:
			p.endResponse()
			p.mode = modeSeek
		}
	}
}

// startItem finishes the item in progress and opens a new one.
func (p *parser) startItem(id, title string) {
	p.flush()
	p.cur = &Item{ID: id, Title: title, Responses: []string{}}
	p.mode, p.sawMeta = modeMeta, false
}

// meta records one metadata line of the item in progress.
func (p *parser) meta(key, value string) {
	p.sawMeta = true

	switch value = strings.TrimSpace(value); key {
	case "file":
		p.cur.File = value
	case "severity":
		p.cur.Severity = value
	case "status":
		p.cur.Status = value
	}
}

// startResponse finishes the response in progress and opens another
// from the text following "- response:".
func (p *parser) startResponse(first string) {
	p.endResponse()
	p.mode = modeResponse
	p.resp = nil

	if first = strings.TrimSpace(first); first != "" {
		p.resp = append(p.resp, first)
	}
}

// endResponse stores the response in progress, if there is one, as a
// single line.
func (p *parser) endResponse() {
	if p.mode != modeResponse {
		return
	}

	p.cur.Responses = append(
		p.cur.Responses, strings.TrimSpace(strings.Join(p.resp, " ")),
	)
	p.resp = nil
}

// flush finishes the item in progress, trimming trailing blank lines
// from its body, and appends it to the result.
func (p *parser) flush() {
	if p.cur == nil {
		p.body, p.mode = nil, modeSeek

		return
	}

	p.endResponse()

	end := len(p.body)
	for end > 0 && strings.TrimSpace(p.body[end-1]) == "" {
		end--
	}
	p.cur.Body = strings.Join(p.body[:end], "\n")

	p.items = append(p.items, *p.cur)
	p.cur, p.body, p.mode = nil, nil, modeSeek
}
