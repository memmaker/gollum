package main

import (
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// gitStatusLine reports the current branch and the uncommitted diff tally
// (lines added/removed against HEAD), for the statusbar's right side. Empty
// when the working directory is not a git repo.
func gitStatusLine() string {
	branch, err := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(branch))

	// No HEAD yet (empty repo) or a clean tree: diff --shortstat fails or
	// prints nothing, and the branch name alone is still worth showing.
	if out, err := exec.Command("git", "diff", "--shortstat", "HEAD").Output(); err == nil {
		if added, removed, ok := parseShortstat(string(out)); ok {
			line += " " + accentStyle.Render("+"+strconv.Itoa(added)) + " " + errStyle.Render("-"+strconv.Itoa(removed))
		}
	}
	return line
}

var shortstatRe = regexp.MustCompile(`(\d+) insertion|(\d+) deletion`)

// parseShortstat pulls insertion/deletion counts out of `git diff
// --shortstat` output, e.g. " 2 files changed, 12 insertions(+), 3 deletions(-)".
// ok is false for a clean tree, whose output is empty.
func parseShortstat(s string) (added, removed int, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, false
	}
	for _, m := range shortstatRe.FindAllStringSubmatch(s, -1) {
		switch {
		case m[1] != "":
			added, _ = strconv.Atoi(m[1])
		case m[2] != "":
			removed, _ = strconv.Atoi(m[2])
		}
	}
	return added, removed, true
}
