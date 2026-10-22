package runner

import (
	"strings"
	"testing"

	"github.com/LXXero/xerotty/internal/protocol"
)

func connectTestTabs() []connectTab {
	return []connectTab{
		{ID: 1, Title: "zsh", Fg: "zsh"},
		{ID: 2, Name: "build", Title: "make -j8", Fg: "make"},
		{ID: 3, Title: "vim connect.go", Fg: "vim"},
		{ID: 4, Name: "logs", Title: "tail -f build.log", Fg: "tail"},
		{ID: 12, Title: "Build server", Fg: "python3"},
	}
}

func TestMatchTabByID(t *testing.T) {
	got, err := matchTab(connectTestTabs(), "3")
	if err != nil || got.ID != 3 {
		t.Fatalf("matchTab(\"3\") = %d, %v; want tab 3", got.ID, err)
	}
}

func TestMatchTabNumericNotAnIDFallsBackToTitle(t *testing.T) {
	tabs := []connectTab{{ID: 1, Title: "port 8080"}, {ID: 2, Title: "zsh"}}
	got, err := matchTab(tabs, "8080")
	if err != nil || got.ID != 1 {
		t.Fatalf("matchTab(\"8080\") = %d, %v; want tab 1 by substring", got.ID, err)
	}
}

func TestMatchTabExactNameBeatsSubstring(t *testing.T) {
	// "build" is a substring of tabs 4 and 12 too, but tab 2's name
	// is exactly "build", so the exact match wins.
	got, err := matchTab(connectTestTabs(), "build")
	if err != nil || got.ID != 2 {
		t.Fatalf("matchTab(\"build\") = %d, %v; want tab 2", got.ID, err)
	}
}

func TestMatchTabExactTitle(t *testing.T) {
	got, err := matchTab(connectTestTabs(), "make -j8")
	if err != nil || got.ID != 2 {
		t.Fatalf("matchTab(\"make -j8\") = %d, %v; want tab 2", got.ID, err)
	}
}

func TestMatchTabUniqueCaseInsensitiveSubstring(t *testing.T) {
	got, err := matchTab(connectTestTabs(), "CONNECT")
	if err != nil || got.ID != 3 {
		t.Fatalf("matchTab(\"CONNECT\") = %d, %v; want tab 3", got.ID, err)
	}
}

func TestMatchTabByForegroundProcess(t *testing.T) {
	// A fresh tab has no name or title; --list labels it by its
	// foreground process, so --tab must find it by that too.
	tabs := []connectTab{{ID: 1, Fg: "zsh"}, {ID: 2, Fg: "htop"}}
	got, err := matchTab(tabs, "zsh")
	if err != nil || got.ID != 1 {
		t.Fatalf("matchTab(\"zsh\") = %d, %v; want tab 1", got.ID, err)
	}
	got, err = matchTab(connectTestTabs(), "PYTHON")
	if err != nil || got.ID != 12 {
		t.Fatalf("matchTab(\"PYTHON\") = %d, %v; want tab 12", got.ID, err)
	}
}

func TestMatchTabAmbiguousListsCandidates(t *testing.T) {
	_, err := matchTab(connectTestTabs(), "buil")
	if err == nil {
		t.Fatal("matchTab(\"buil\") matched; want an ambiguity error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "ambiguous") {
		t.Fatalf("error %q does not say ambiguous", msg)
	}
	for _, want := range []string{"2\tbuild", "4\tlogs", "12\tBuild server"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q is missing candidate %q", msg, want)
		}
	}
	if strings.Contains(msg, "zsh") {
		t.Fatalf("error %q lists a tab that does not match", msg)
	}
}

func TestMatchTabAmbiguousExact(t *testing.T) {
	tabs := []connectTab{{ID: 1, Title: "zsh"}, {ID: 2, Title: "zsh"}}
	if _, err := matchTab(tabs, "zsh"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("two exact title matches gave err=%v; want ambiguous", err)
	}
}

func TestMatchTabMissingListsAllTabs(t *testing.T) {
	_, err := matchTab(connectTestTabs(), "nope")
	if err == nil {
		t.Fatal("matchTab(\"nope\") matched; want an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "no tab matches") {
		t.Fatalf("error %q does not say no match", msg)
	}
	for _, want := range []string{"1\tzsh", "2\tbuild", "3\tvim connect.go", "4\tlogs", "12\tBuild server"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q is missing tab %q", msg, want)
		}
	}
}

func TestMatchTabMissingID(t *testing.T) {
	if _, err := matchTab(connectTestTabs(), "99"); err == nil {
		t.Fatal("matchTab(\"99\") matched; want an error")
	}
}

func TestFormatTabList(t *testing.T) {
	a := &protocol.Attached{Tabs: []protocol.TabInfo{
		{ID: 1, Title: "zsh"},
		{ID: 2, Name: "build", Title: "make -j8"},
		{ID: 3, Title: "vim connect.go"},
	}}
	tabs := tabsFromAttached(a)
	mergeTabState(tabs, &protocol.TabState{ID: 1, CWD: "/home/xero", ForegroundProcessName: "zsh"})
	mergeTabState(tabs, &protocol.TabState{ID: 2, CWD: "/home/xero/git/xerotty", ForegroundProcessName: "make", Title: "make -j8 all"})
	// Tab 3 never reports state: cwd and process print as "-".

	got := formatTabList(tabs, 2)
	want := "  1  zsh             /home/xero              zsh\n" +
		"* 2  build           /home/xero/git/xerotty  make\n" +
		"  3  vim connect.go  -                       -\n"
	if got != want {
		t.Fatalf("formatTabList:\n%s\nwant:\n%s", got, want)
	}
}

func TestFormatTabListLabelFallsBackToProcess(t *testing.T) {
	tabs := []connectTab{{ID: 7, Fg: "htop", CWD: "/"}}
	got := formatTabList(tabs, 0)
	if got != "  7  htop  /  htop\n" {
		t.Fatalf("formatTabList = %q", got)
	}
}

func TestMergeTabStateKeepsTopologyLabels(t *testing.T) {
	tabs := []connectTab{{ID: 2, Name: "build", Title: "make"}}
	if !mergeTabState(tabs, &protocol.TabState{ID: 2, CWD: "/tmp"}) {
		t.Fatal("mergeTabState did not find tab 2")
	}
	if tabs[0].Name != "build" || tabs[0].Title != "make" || tabs[0].CWD != "/tmp" {
		t.Fatalf("merged row = %+v", tabs[0])
	}
	if mergeTabState(tabs, &protocol.TabState{ID: 9}) {
		t.Fatal("mergeTabState reported a match for an unknown tab")
	}
}
