package app

import (
	"os/exec"
	"regexp"
	"strings"

	"github.com/charmbracelet/x/vt"
)

// urlPattern matches common URLs in terminal output.
var urlPattern = regexp.MustCompile(`https?://[^\s<>"'\x60\x7f-\x9f\]})]+`)

// linkHit holds the URL found under a screen position.
type linkHit struct {
	URL      string
	StartCol int
	EndCol   int
	Row      int
}

// detectLinkAt scans the line at the given viewport row for a URL under col.
// scrollOffset accounts for scrollback position.
func detectLinkAt(emu *vt.SafeEmulator, col, row, scrollOffset int) *linkHit {
	cols := emu.Width()
	line := extractLineText(emu, row, scrollOffset, cols)

	for _, loc := range urlPattern.FindAllStringIndex(line, -1) {
		if col >= loc[0] && col < loc[1] {
			return &linkHit{
				URL:      line[loc[0]:loc[1]],
				StartCol: loc[0],
				EndCol:   loc[1] - 1,
				Row:      row,
			}
		}
	}
	return nil
}

// extractLineText builds a string from a viewport row's cell contents.
func extractLineText(emu *vt.SafeEmulator, row, scrollOffset, cols int) string {
	var b strings.Builder
	b.Grow(cols)

	sbLen := emu.ScrollbackLen()
	contentIdx := sbLen - scrollOffset + row

	for col := 0; col < cols; col++ {
		var content string
		if contentIdx < sbLen {
			cell := emu.ScrollbackCellAt(col, contentIdx)
			if cell != nil {
				content = cell.Content
			}
		} else {
			cell := emu.CellAt(col, contentIdx-sbLen)
			if cell != nil {
				content = cell.Content
			}
		}
		if content == "" {
			b.WriteByte(' ')
		} else {
			b.WriteString(content)
		}
	}
	return b.String()
}

// openURL opens a URL with the given opener command.
func openURL(url, opener string) {
	if opener == "" {
		opener = "xdg-open"
	}
	cmd := exec.Command(opener, url)
	_ = cmd.Start()
}
