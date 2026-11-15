package memreport

import (
	"encoding/json"
	"os"
	"runtime"
	"runtime/debug"
	"testing"

	"github.com/LXXero/xerotty/internal/config"
)

// saveGC restores the runtime's GC percent and memory limit, and this
// package's record of their sources, when the test ends. It also
// clears GOGC and GOMEMLIMIT for the test.
func saveGC(t *testing.T) {
	t.Helper()
	pct := debug.SetGCPercent(100)
	debug.SetGCPercent(pct)
	lim := debug.SetMemoryLimit(-1)
	t.Cleanup(func() {
		debug.SetGCPercent(pct)
		debug.SetMemoryLimit(lim)
		limitSource, gcPercentSource = "none", "runtime"
	})
	for _, k := range []string{"GOGC", "GOMEMLIMIT"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

// runtimeGCPercent reads the percent the runtime really holds, through
// debug.SetGCPercent's return value and not this package's reader.
func runtimeGCPercent() int {
	p := debug.SetGCPercent(100)
	debug.SetGCPercent(p)
	return p
}

// The functions the two start paths call (cmd/xerotty: ApplyGUI;
// `xerotty serve`: ApplyDaemon), given the default config.
func TestStartPathsApplyDefaults(t *testing.T) {
	def := config.Default().Memory
	if def.GUILimitMB != 0 || def.DaemonLimitMB != 0 {
		t.Fatalf("default limits = %d, %d; want 0, 0", def.GUILimitMB, def.DaemonLimitMB)
	}
	for name, apply := range map[string]func(config.MemoryConfig) Settings{"gui": ApplyGUI, "daemon": ApplyDaemon} {
		t.Run(name, func(t *testing.T) {
			saveGC(t)
			debug.SetGCPercent(100)
			debug.SetMemoryLimit(123 << 20)
			s := apply(def)
			if s.GCPercent != 50 || s.GCPercentSource != "config" || runtimeGCPercent() != 50 {
				t.Fatalf("GC percent = %d (%s), runtime %d; want 50 from config", s.GCPercent, s.GCPercentSource, runtimeGCPercent())
			}
			if s.LimitBytes != 0 || s.LimitSource != "none" || debug.SetMemoryLimit(-1) != noLimit {
				t.Fatalf("limit = %d (%s); want none", s.LimitBytes, s.LimitSource)
			}
			r := Read("test", false)
			if r.GCPercent != 50 || r.GCPercentSource != "config" || r.MemoryLimitBytes != 0 {
				t.Fatalf("report: percent %d (%s), limit %d", r.GCPercent, r.GCPercentSource, r.MemoryLimitBytes)
			}
		})
	}
}

// Each process reads its own settings, and a config value overrides
// the default.
func TestStartPathsApplyConfig(t *testing.T) {
	m := config.MemoryConfig{GUIGCPercent: 75, DaemonGCPercent: 30, GUILimitMB: 300, DaemonLimitMB: 400}
	saveGC(t)
	if s := ApplyGUI(m); s.GCPercent != 75 || s.GCPercentSource != "config" || runtimeGCPercent() != 75 ||
		s.LimitBytes != 300<<20 || s.LimitSource != "config" {
		t.Fatalf("ApplyGUI = %+v, runtime percent %d", s, runtimeGCPercent())
	}
	if s := ApplyDaemon(m); s.GCPercent != 30 || runtimeGCPercent() != 30 ||
		s.LimitBytes != 400<<20 || debug.SetMemoryLimit(-1) != 400<<20 {
		t.Fatalf("ApplyDaemon = %+v, runtime percent %d", s, runtimeGCPercent())
	}
}

// GOGC and GOMEMLIMIT win: the runtime applied them at start (stood in
// for here by setting the values directly) and the config values must
// not replace them.
func TestEnvironmentWins(t *testing.T) {
	saveGC(t)
	debug.SetGCPercent(200)
	debug.SetMemoryLimit(700 << 20)
	t.Setenv("GOGC", "200")
	t.Setenv("GOMEMLIMIT", "700MiB")
	s := ApplyGUI(config.MemoryConfig{GUIGCPercent: 50, GUILimitMB: 300})
	if s.GCPercent != 200 || s.GCPercentSource != "GOGC" || runtimeGCPercent() != 200 {
		t.Fatalf("with GOGC=200: %+v, runtime %d", s, runtimeGCPercent())
	}
	if s.LimitBytes != 700<<20 || s.LimitSource != "GOMEMLIMIT" {
		t.Fatalf("with GOMEMLIMIT: %+v", s)
	}

	// GOGC=off stays off, and the report says so.
	debug.SetGCPercent(-1)
	t.Setenv("GOGC", "off")
	if s := ApplyDaemon(config.Default().Memory); s.GCPercent != -1 || s.GCPercentSource != "GOGC" || runtimeGCPercent() != -1 {
		t.Fatalf("with GOGC=off: %+v, runtime %d", s, runtimeGCPercent())
	}
}

// A config value of 0 or less must not turn the collector off or make
// it run continuously: the default is used.
func TestInvalidGCPercentUsesDefault(t *testing.T) {
	for _, v := range []int{0, -1, -100} {
		saveGC(t)
		debug.SetGCPercent(100)
		s := ApplyDaemon(config.MemoryConfig{DaemonGCPercent: v})
		if s.GCPercent != config.DefaultGCPercent || s.GCPercentSource != "default" || runtimeGCPercent() != config.DefaultGCPercent {
			t.Fatalf("daemon_gc_percent = %d: %+v, runtime %d", v, s, runtimeGCPercent())
		}
	}
}

func TestReportCarriesGCCPUTime(t *testing.T) {
	saveGC(t)
	// Make garbage and collect, so the collector has used CPU time.
	var keep [][]byte
	for i := 0; i < 20; i++ {
		for j := 0; j < 2000; j++ {
			keep = append(keep, make([]byte, 4096))
		}
		keep = keep[:0]
		runtime.GC()
	}
	r := Read("test", true)
	if r.GCCPUSeconds <= 0 {
		t.Fatalf("gc_cpu_seconds = %v, want > 0", r.GCCPUSeconds)
	}
	if r.ProcessCPUSeconds <= 0 || r.GCCPUShare <= 0 || r.GCCPUShare != r.GCCPUSeconds/r.ProcessCPUSeconds {
		t.Fatalf("process_cpu_seconds = %v, gc_cpu_share = %v", r.ProcessCPUSeconds, r.GCCPUShare)
	}
	b, _ := json.Marshal(r)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for _, k := range []string{"gc_cpu_seconds", "process_cpu_seconds", "gc_cpu_share", "gc_percent", "gc_percent_source"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("report JSON has no %q: %s", k, b)
		}
	}
}

func TestApplyLimit(t *testing.T) {
	prev := debug.SetMemoryLimit(-1)
	defer func() { debug.SetMemoryLimit(prev); limitSource = "none" }()

	// The config setting is applied.
	t.Setenv("GOMEMLIMIT", "")
	os.Unsetenv("GOMEMLIMIT")
	lim, src := ApplyLimit(300)
	if lim != 300<<20 || src != "config" {
		t.Fatalf("ApplyLimit(300) = %d, %q; want %d, config", lim, src, 300<<20)
	}
	if got := debug.SetMemoryLimit(-1); got != 300<<20 {
		t.Fatalf("runtime limit = %d, want %d", got, 300<<20)
	}
	if r := Read("test", false); r.MemoryLimitBytes != 300<<20 || r.LimitSource != "config" {
		t.Fatalf("report limit = %d (%s)", r.MemoryLimitBytes, r.LimitSource)
	}

	// GOMEMLIMIT wins: the runtime applied it at start (stood in for
	// here by setting the limit directly), and the config value must
	// not replace it.
	debug.SetMemoryLimit(700 << 20)
	t.Setenv("GOMEMLIMIT", "700MiB")
	lim, src = ApplyLimit(300)
	if lim != 700<<20 || src != "GOMEMLIMIT" {
		t.Fatalf("with GOMEMLIMIT: ApplyLimit(300) = %d, %q; want %d, GOMEMLIMIT", lim, src, 700<<20)
	}

	// 0 means no limit.
	os.Unsetenv("GOMEMLIMIT")
	debug.SetMemoryLimit(noLimit)
	lim, src = ApplyLimit(0)
	if lim != 0 || src != "none" {
		t.Fatalf("ApplyLimit(0) = %d, %q; want 0, none", lim, src)
	}
}

func TestReportAndProfile(t *testing.T) {
	r, err := ReportWithProfile("test", t.TempDir(), true, true)
	if err != nil {
		t.Fatal(err)
	}
	if r.LiveHeapBytes == 0 || r.HeapInUseBytes == 0 || r.ProfilePath == "" {
		t.Fatalf("report incomplete: %+v", r)
	}
	fi, err := os.Stat(r.ProfilePath)
	if err != nil || fi.Size() == 0 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("profile file: %v %v", fi, err)
	}
}
