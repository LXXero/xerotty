package app

import (
	"os/exec"
	"regexp"
	"strings"

	"github.com/LXXero/xerotty/internal/config"
	uv "github.com/charmbracelet/ultraviolet"
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
// linkGrid is the read surface link detection needs — satisfied by
// both in-process Terminals and daemon Sources. Hit-testing MUST go
// through this (not the raw shadow emulator): daemon tabs mirror
// scrollback in the Source ring and pty tabs can spill it to disk,
// so the raw emulator's ScrollbackLen is 0/partial and scrolled-up
// rows computed negative content indices — links silently died
// anywhere above the live screen.
type linkGrid interface {
	Width() int
	ScrollbackLen() int
	CellAt(col, row int) *uv.Cell
	ScrollbackCellAt(col, row int) *uv.Cell
}

func detectLinkAt(emu linkGrid, col, row, scrollOffset int) *linkHit {
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
func extractLineText(emu linkGrid, row, scrollOffset, cols int) string {
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
		opener = config.DefaultOpener()
	}
	cmd := exec.Command(opener, url)
	_ = cmd.Start()
}
