package runner

import (
	"strconv"
	"strings"
)

// lsofTxt reads `lsof -F Din` output for one process's txt files and
// reports whether one of them is id, plus the first one's path (the
// executable), for messages. It is the macOS mapsFile's parser; it
// carries no build tag so its tests run everywhere. As on Linux a
// file matches on inode plus device or path.
func lsofTxt(out []byte, id fileID) (ok bool, mapped string) {
	var (
		dev, ino       uint64
		hasDev, hasIno bool
		name           string
	)
	flush := func() {
		if mapped == "" {
			mapped = name
		}
		if hasIno && ino == id.ino && ((hasDev && dev == id.dev) || name == id.path) {
			ok = true
		}
		dev, ino, hasDev, hasIno, name = 0, 0, false, false, ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		v := line[1:]
		switch line[0] {
		case 'p', 'f':
			// A new process or file starts.
			flush()
		case 'D':
			dev, hasDev = parseUint(v, 0)
		case 'i':
			ino, hasIno = parseUint(v, 10)
		case 'n':
			name = v
		}
	}
	flush()
	return ok, mapped
}

func parseUint(s string, base int) (uint64, bool) {
	n, err := strconv.ParseUint(s, base, 64)
	return n, err == nil
}
