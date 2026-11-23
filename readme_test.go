package h_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	readmeMarker = regexp.MustCompile(`^<!-- example: (\S+) -->$`)
	regionBegin  = regexp.MustCompile(`^(\s*)// README begin (\S+)$`)
	regionEnd    = regexp.MustCompile(`^\s*// README end$`)
)

func TestREADMEGoBlocksMatchTheirRegions(t *testing.T) {
	regions := readmeRegions(t)
	used := map[string]bool{}
	for _, b := range readmeGoBlocks(t) {
		switch b.name {
		case "":
			t.Errorf("README.md:%d: Go block has no <!-- example: NAME --> or <!-- example: none --> marker", b.line)
		case "none":
		default:
			want, ok := regions[b.name]
			if !ok {
				t.Errorf("README.md:%d: no region %q; mark it in a .go file with // README begin %s", b.line, b.name, b.name)
				continue
			}
			used[b.name] = true
			if b.text != want {
				t.Errorf("README.md:%d: block differs from region %q\nREADME:\n%s\nregion:\n%s", b.line, b.name, b.text, want)
			}
		}
	}
	for name := range regions {
		if !used[name] {
			t.Errorf("region %q is not shown in README.md", name)
		}
	}
}

type readmeBlock struct {
	name, text string
	line       int
}

func readmeGoBlocks(t *testing.T) []readmeBlock {
	t.Helper()
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	var blocks []readmeBlock
	var cur *readmeBlock
	var marker string
	for i, line := range lines(readme) {
		switch {
		case cur != nil && line == "```":
			blocks = append(blocks, *cur)
			cur = nil
		case cur != nil:
			cur.text += line + "\n"
		case line == "```go":
			cur = &readmeBlock{name: marker, line: i + 1}
			marker = ""
		case strings.HasPrefix(strings.TrimSpace(line), "```go"):
			t.Errorf("README.md:%d: Go block fence %q is not checked; open it with ```go at the start of the line", i+1, line)
		case readmeMarker.MatchString(line):
			marker = readmeMarker.FindStringSubmatch(line)[1]
		case strings.TrimSpace(line) != "":
			marker = ""
		}
	}
	if cur != nil {
		t.Errorf("README.md:%d: Go block is not closed", cur.line)
	}
	return blocks
}

// lines splits b into lines without their line endings, LF or CRLF.
func lines(b []byte) []string {
	return strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
}

func readmeRegions(t *testing.T) map[string]string {
	t.Helper()
	regions := map[string]string{}
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && strings.HasPrefix(d.Name(), ".") && path != "." {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var name, indent, text string
		open := false
		for i, line := range lines(b) {
			switch {
			case regionBegin.MatchString(line):
				if open {
					t.Errorf("%s:%d: region %q opened inside region %q", path, i+1, regionBegin.FindStringSubmatch(line)[2], name)
				}
				m := regionBegin.FindStringSubmatch(line)
				indent, name, text, open = m[1], m[2], "", true
				if _, dup := regions[name]; dup {
					t.Errorf("%s:%d: region %q declared twice", path, i+1, name)
				}
			case open && regionEnd.MatchString(line):
				// gofmt keeps a blank line between a declaration and a
				// comment next to it, such as a marker.
				regions[name], open = strings.Trim(text, "\n")+"\n", false
			case open:
				if line != "" && !strings.HasPrefix(line, indent) {
					t.Errorf("%s:%d: line of region %q is indented less than its marker", path, i+1, name)
				}
				text += strings.TrimPrefix(line, indent) + "\n"
			}
		}
		if open {
			t.Errorf("%s: region %q is not closed", path, name)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return regions
}
