package tasklint

import (
	"regexp"
	"slices"
	"strings"
)

// statusRe matches the status line that follows the H1.
var statusRe = regexp.MustCompile(`^Status: (\S+)`)

// Doc is a parsed task file.
type Doc struct {
	// Title is the H1 text without its "# ".
	Title string
	// Status is the value of the status line under the H1, or "".
	Status string
	// Order lists section names in document order, first occurrence
	// only.
	Order []string
	// Sections maps a section name to its body: the lines between
	// its heading and the next one, each keeping its newline. The
	// first occurrence of a name wins.
	Sections map[string]string
}

// Parse scans text line by line. A line starting with "```" toggles
// fenced mode, and inside a fence no line is a heading, an H1, or a
// status line. A repeated "## " heading does not open a section: its
// line and the lines after it stay with the section it interrupts.
// Only the first non-empty line after the H1 can carry the status,
// whatever else that line turns out to be: a heading or a fence
// there leaves Status empty rather than passing the search on to a
// later line.
func Parse(text string) Doc {
	d := Doc{Sections: map[string]string{}}

	var (
		fenced     bool
		section    string
		body       strings.Builder
		afterTitle bool
	)

	flush := func() {
		if section != "" {
			d.Sections[section] = body.String()
		}
		body.Reset()
	}

	for _, line := range strings.Split(
		strings.TrimSuffix(text, "\n"), "\n",
	) {
		if afterTitle && strings.TrimSpace(line) != "" {
			afterTitle = false
			if m := statusRe.FindStringSubmatch(line); m != nil {
				d.Status = m[1]
			}
		}

		switch {
		case strings.HasPrefix(line, "```"):
			fenced = !fenced
		case fenced:
		case isHeading(line) && !slices.Contains(d.Order, name(line)):
			flush()
			section = name(line)
			d.Order = append(d.Order, section)

			continue
		case d.Title == "" && strings.HasPrefix(line, "# "):
			d.Title = strings.TrimSpace(line[len("# "):])
			afterTitle = true

			continue
		}

		if section != "" {
			body.WriteString(line)
			body.WriteByte('\n')
		}
	}
	flush()

	return d
}

// isHeading reports whether line is a "## " section heading with a
// name.
func isHeading(line string) bool {
	return strings.HasPrefix(line, "## ") && name(line) != ""
}

// name returns the trimmed name of a "## " heading line.
func name(line string) string {
	return strings.TrimSpace(strings.TrimPrefix(line, "## "))
}

// Section returns the body of the named section, or "" when the
// document has none.
func (d Doc) Section(sec string) string { return d.Sections[sec] }

// Judgeable reports whether d holds the two sections the model
// questions read: without an Objective or an Acceptance there is
// nothing to judge.
func (d Doc) Judgeable() bool {
	for _, sec := range []string{"Objective", "Acceptance"} {
		if _, ok := d.Sections[sec]; !ok {
			return false
		}
	}

	return true
}
