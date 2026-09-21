package tasklint

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// required lists the headings the template demands, in template
// order.
var required = []string{
	"Objective", "Files", "Out of scope", "Acceptance",
	"Budget", "Rules", "Result", "Verification",
}

// statuses are the values a status line may carry.
var statuses = []string{"ready", "in-progress", "done", "accepted"}

// budgetRe matches a turn count in the Budget body.
var budgetRe = regexp.MustCompile(`\b\d+ turns\b`)

// Finding is one static check result.
type Finding struct {
	Check  string `json:"check"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// Static runs the structural checks that need no model, always in
// the order listed, always returning all six.
func Static(d Doc) []Finding {
	return []Finding{
		statusLine(d),
		sectionsPresent(d),
		sectionsOrdered(d),
		allowedNonempty(d),
		acceptanceCommand(d),
		budgetNumeric(d),
	}
}

// statusLine checks that the document declares a known status.
func statusLine(d Doc) Finding {
	if slices.Contains(statuses, d.Status) {
		return Finding{Check: "status_line", OK: true}
	}

	return Finding{Check: "status_line", Detail: fmt.Sprintf(
		"status %q is not one of %s",
		d.Status, strings.Join(statuses, "|"),
	)}
}

// sectionsPresent checks that every required heading exists. Its
// detail is the missing names alone, comma-separated, in template
// order.
func sectionsPresent(d Doc) Finding {
	var missing []string
	for _, sec := range required {
		if _, ok := d.Sections[sec]; !ok {
			missing = append(missing, sec)
		}
	}

	if len(missing) == 0 {
		return Finding{Check: "sections_present", OK: true}
	}

	return Finding{
		Check:  "sections_present",
		Detail: strings.Join(missing, ", "),
	}
}

// sectionsOrdered checks that the required headings that are present
// keep their template order. Missing headings are ignored: their
// absence is sectionsPresent's business.
func sectionsOrdered(d Doc) Finding {
	const check = "sections_ordered"

	rank, previous := -1, ""
	for _, sec := range d.Order {
		i := slices.Index(required, sec)
		if i < 0 {
			continue
		}
		if i < rank {
			return Finding{Check: check, Detail: fmt.Sprintf(
				"%s appears after %s", sec, previous,
			)}
		}
		rank, previous = i, sec
	}

	return Finding{Check: check, OK: true}
}

// allowedNonempty checks that the Files body names at least one
// allowed path.
func allowedNonempty(d Doc) Finding {
	const check = "allowed_nonempty"

	lines := unfenced(d.Section("Files"))
	i := slices.IndexFunc(lines, func(line string) bool {
		return strings.HasPrefix(line, "Allowed:")
	})
	if i < 0 {
		return Finding{Check: check, Detail: "no Allowed: line"}
	}

	list := []string{strings.TrimPrefix(lines[i], "Allowed:")}
	for _, line := range lines[i+1:] {
		if strings.TrimSpace(line) == "" ||
			strings.HasPrefix(line, "Read-only:") {
			break
		}
		list = append(list, line)
	}

	if len(paths(list)) == 0 {
		return Finding{Check: check, Detail: "Allowed: names no paths"}
	}

	return Finding{Check: check, OK: true}
}

// paths splits the allowed list, which is comma-separated and may
// wrap across lines, into its non-empty trimmed tokens.
func paths(lines []string) []string {
	var out []string
	for _, tok := range strings.Split(strings.Join(lines, ","), ",") {
		if tok = strings.TrimSpace(tok); tok != "" {
			out = append(out, tok)
		}
	}

	return out
}

// acceptanceCommand checks that the Acceptance body holds at least
// one Command: line and that every one of them is answered by an
// Expect: line before the next command.
func acceptanceCommand(d Doc) Finding {
	const check = "acceptance_command"

	lines := unfenced(d.Section("Acceptance"))
	commands := 0
	for i, line := range lines {
		if !strings.HasPrefix(line, "Command:") {
			continue
		}
		commands++
		if !hasExpect(lines[i+1:]) {
			return Finding{Check: check, Detail: fmt.Sprintf(
				"Command %d has no Expect: line", commands,
			)}
		}
	}

	if commands == 0 {
		return Finding{Check: check, Detail: "no Command: line"}
	}

	return Finding{Check: check, OK: true}
}

// hasExpect reports whether lines open with an Expect: line before
// the next Command: line.
func hasExpect(lines []string) bool {
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "Expect:"):
			return true
		case strings.HasPrefix(line, "Command:"):
			return false
		}
	}

	return false
}

// budgetNumeric checks that the Budget body states a turn count.
func budgetNumeric(d Doc) Finding {
	const check = "budget_numeric"

	if budgetRe.MatchString(d.Section("Budget")) {
		return Finding{Check: check, OK: true}
	}

	return Finding{
		Check:  check,
		Detail: `no "<n> turns" in the budget`,
	}
}

// unfenced splits body into lines and drops the fences and the lines
// inside them, so an example in a code block cannot satisfy a check.
func unfenced(body string) []string {
	var (
		out    []string
		fenced bool
	)

	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "```") {
			fenced = !fenced

			continue
		}
		if !fenced {
			out = append(out, line)
		}
	}

	return out
}
