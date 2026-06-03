package daemon

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/LXXero/xerotty/internal/config"
)

// TestFindOrCreateTabReuse exercises named-tab idempotency end to end:
// the same name returns the same LIVE tab, and a tab whose shell has
// reaped is refused (recreated) rather than handed back as a corpse.
// Drives the session directly (no daemon.Run) like the subscribe
// internal test.
func TestFindOrCreateTabReuse(t *testing.T) {
	cfg := config.Default()
	d := New(&cfg, filepath.Join(t.TempDir(), "xerottyd.sock"))
	sess := d.session("default")

	// First call spawns and tags the tab.
	t1, _, created, err := sess.FindOrCreateTab("build", 0, 80, 24, "")
	if err != nil {
		t.Fatalf("first FindOrCreateTab: %v", err)
	}
	if !created {
		t.Fatal("first call must report created=true")
	}

	// Second call with the same name reuses the SAME tab without
	// spawning.
	t2, _, created, err := sess.FindOrCreateTab("build", 0, 80, 24, "")
	if err != nil {
		t.Fatalf("second FindOrCreateTab: %v", err)
	}
	if created {
		t.Error("second call must reuse (created=false)")
	}
	if t2.ID != t1.ID {
		t.Errorf("reuse returned tab %d, want same tab %d", t2.ID, t1.ID)
	}

	// Kill the shell and wait for the reap so the named tab becomes a
	// corpse still indexed in tabsByName (child-exit doesn't unlink).
	if _, err := t1.Term.Write([]byte("exit\r")); err != nil {
		t.Fatalf("write exit: %v", err)
	}
	select {
	case <-t1.Exited:
	case <-time.After(5 * time.Second):
		t.Fatal("shell did not exit within 5s")
	}

	// Third call must NOT hand back the corpse — it drops the stale
	// label and spawns a fresh tab under the same name.
	t3, _, created, err := sess.FindOrCreateTab("build", 0, 80, 24, "")
	if err != nil {
		t.Fatalf("third FindOrCreateTab: %v", err)
	}
	if !created {
		t.Error("dead-shell reuse must recreate (created=true)")
	}
	if t3.ID == t1.ID {
		t.Errorf("reused dead tab %d; expected a fresh tab", t1.ID)
	}
}
