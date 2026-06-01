package app

import "testing"

// TestReseatNeedsMint is the finding-2 regression: the reseat mint-once
// must be scoped to a single daemon instance, so a SECOND restart while
// a reseat is still pending re-mints the daemon window on the new daemon
// instead of clinging to the dead intermediate's stale window ID.
//
// The frame() loop that uses this isn't unit-testable (it drives ImGui),
// so the decision is factored into reseatNeedsMint and tested directly.
func TestReseatNeedsMint(t *testing.T) {
	cases := []struct {
		name            string
		minted          bool
		mintedInstance  string
		currentInstance string
		want            bool
	}{
		{"nothing minted yet", false, "", "", true},
		{"not minted, instances known", false, "inst1", "inst1", true},
		{"minted, same instance — reuse", true, "inst1", "inst1", false},
		{"minted, instance changed (2nd restart) — re-mint", true, "inst1", "inst2", true},
		{"minted, current unknown — keep (safe degrade)", true, "inst1", "", false},
		{"minted against unknown, now known — re-mint", true, "", "inst2", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reseatNeedsMint(tc.minted, tc.mintedInstance, tc.currentInstance)
			if got != tc.want {
				t.Fatalf("reseatNeedsMint(%v, %q, %q) = %v, want %v",
					tc.minted, tc.mintedInstance, tc.currentInstance, got, tc.want)
			}
		})
	}
}
