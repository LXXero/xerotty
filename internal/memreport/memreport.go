// Package memreport reports a process's Go heap, resident size and GC
// CPU time, and applies the configured GC settings ([memory] in the
// config: GC percent and the optional memory limit). Both the daemon and the GUI
// use it, through their MCP sockets (get_heap_report); it opens no
// network port.
package memreport

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"runtime/pprof"
	"strconv"
	"strings"
	"time"

	"github.com/LXXero/xerotty/internal/config"
)

// Report is one reading of the process's memory. Byte counts come from
// runtime.MemStats; ResidentBytes from /proc/self/statm (0 where that
// file does not exist).
type Report struct {
	Role string `json:"role"`
	PID  int    `json:"pid"`
	// LiveHeapBytes is MemStats.HeapAlloc: bytes in reachable objects
	// plus unswept garbage. With GC true a collection ran just before
	// the reading, so it is the live heap.
	LiveHeapBytes     uint64 `json:"live_heap_bytes"`
	HeapInUseBytes    uint64 `json:"heap_in_use_bytes"`
	HeapIdleBytes     uint64 `json:"heap_idle_bytes"`
	HeapReleasedBytes uint64 `json:"heap_released_bytes"`
	HeapSysBytes      uint64 `json:"heap_sys_bytes"`
	StackInUseBytes   uint64 `json:"stack_in_use_bytes"`
	GoSysBytes        uint64 `json:"go_sys_bytes"`
	NextGCBytes       uint64 `json:"next_gc_bytes"`
	NumGC             uint32 `json:"num_gc"`
	Goroutines        int    `json:"goroutines"`
	ResidentBytes     uint64 `json:"resident_bytes"`
	// MemoryLimitBytes is the GC memory limit in effect; 0 = no limit.
	MemoryLimitBytes int64 `json:"memory_limit_bytes"`
	// LimitSource says where the limit came from: "GOMEMLIMIT",
	// "config" or "none".
	LimitSource string `json:"limit_source"`
	// GCPercent is the GC percent in effect (the runtime's
	// /gc/gogc:percent; -1 = collector off). GCPercentSource says
	// where it came from: "GOGC", "config", "default" (the config
	// value was 0 or negative, so config.DefaultGCPercent was used) or
	// "runtime" (Apply was never called).
	GCPercent       int64  `json:"gc_percent"`
	GCPercentSource string `json:"gc_percent_source"`
	// GCCPUSeconds is the CPU time the collector has used since the
	// process started (runtime/metrics /cpu/classes/gc/total:cpu-seconds,
	// an estimate the runtime updates at each collection).
	// ProcessCPUSeconds is the process's user plus system CPU time
	// from getrusage (0 where that is not available), and GCCPUShare
	// is GCCPUSeconds / ProcessCPUSeconds.
	GCCPUSeconds      float64 `json:"gc_cpu_seconds"`
	ProcessCPUSeconds float64 `json:"process_cpu_seconds"`
	GCCPUShare        float64 `json:"gc_cpu_share"`
	GC                bool    `json:"gc_before_reading"`
	// ProfilePath is the heap profile written for this report, if one
	// was requested.
	ProfilePath string `json:"profile_path,omitempty"`
}

var limitSource = "none"

// noLimit is what debug.SetMemoryLimit reports when no limit is set.
const noLimit = int64(^uint64(0) >> 1)

// ApplyLimit sets the GC memory limit from a config value in MiB. The
// GOMEMLIMIT environment variable wins when it is set (the runtime
// already applied it); 0 or less means no limit. It returns the limit
// in effect in bytes (0 = none) and its source.
func ApplyLimit(limitMB int) (int64, string) {
	if os.Getenv("GOMEMLIMIT") != "" {
		limitSource = "GOMEMLIMIT"
	} else if limitMB > 0 {
		debug.SetMemoryLimit(int64(limitMB) << 20)
		limitSource = "config"
	} else {
		debug.SetMemoryLimit(noLimit)
		limitSource = "none"
	}
	return currentLimit(), limitSource
}

var gcPercentSource = "runtime"

// ApplyGCPercent sets the GC percent (debug.SetGCPercent) from a
// config value. The GOGC environment variable wins when it is set:
// the runtime already applied it and SetGCPercent is not called. A
// value of 0 or less is not accepted (0 would collect continuously
// and a negative value turns the collector off): it is replaced by
// config.DefaultGCPercent and the source reads "default". To turn the
// collector off, set GOGC=off. It returns the percent in effect and
// its source.
func ApplyGCPercent(percent int) (int64, string) {
	switch {
	case os.Getenv("GOGC") != "":
		gcPercentSource = "GOGC"
	case percent <= 0:
		debug.SetGCPercent(config.DefaultGCPercent)
		gcPercentSource = "default"
	default:
		debug.SetGCPercent(percent)
		gcPercentSource = "config"
	}
	return currentGCPercent(), gcPercentSource
}

// Settings is what Apply put into effect.
type Settings struct {
	LimitBytes      int64 // 0 = no limit
	LimitSource     string
	GCPercent       int64
	GCPercentSource string
}

func (s Settings) String() string {
	lim := "none"
	if s.LimitBytes > 0 {
		lim = fmt.Sprintf("%d MiB (%s)", s.LimitBytes>>20, s.LimitSource)
	}
	return fmt.Sprintf("GC percent %d (%s), memory limit %s", s.GCPercent, s.GCPercentSource, lim)
}

// ApplyGUI applies the GUI process's [memory] settings; cmd/xerotty
// calls it at start.
func ApplyGUI(m config.MemoryConfig) Settings {
	return apply(m.GUILimitMB, m.GUIGCPercent)
}

// ApplyDaemon applies the daemon's [memory] settings; `xerotty serve`
// calls it at start.
func ApplyDaemon(m config.MemoryConfig) Settings {
	return apply(m.DaemonLimitMB, m.DaemonGCPercent)
}

func apply(limitMB, gcPercent int) Settings {
	var s Settings
	s.LimitBytes, s.LimitSource = ApplyLimit(limitMB)
	s.GCPercent, s.GCPercentSource = ApplyGCPercent(gcPercent)
	return s
}

const (
	metricGCPercent = "/gc/gogc:percent"
	metricGCCPU     = "/cpu/classes/gc/total:cpu-seconds"
)

func currentGCPercent() int64 {
	s := []metrics.Sample{{Name: metricGCPercent}}
	metrics.Read(s)
	if s[0].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	v := s[0].Value.Uint64()
	if v > 1<<31 { // the runtime reports "off" as a huge value
		return -1
	}
	return int64(v)
}

// gcCPUSeconds reads the collector's CPU time from runtime/metrics.
func gcCPUSeconds() float64 {
	s := []metrics.Sample{{Name: metricGCCPU}}
	metrics.Read(s)
	if s[0].Value.Kind() != metrics.KindFloat64 {
		return 0
	}
	return s[0].Value.Float64()
}

func currentLimit() int64 {
	l := debug.SetMemoryLimit(-1)
	if l == noLimit {
		return 0
	}
	return l
}

// Read returns the current memory reading. gc runs a collection first
// so LiveHeapBytes holds only reachable objects.
func Read(role string, gc bool) Report {
	if gc {
		runtime.GC()
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	src := limitSource
	if src == "none" && os.Getenv("GOMEMLIMIT") != "" {
		src = "GOMEMLIMIT"
	}
	gcCPU, procCPU := gcCPUSeconds(), processCPUSeconds()
	share := 0.0
	if procCPU > 0 {
		share = gcCPU / procCPU
	}
	return Report{
		GCPercent: currentGCPercent(), GCPercentSource: gcPercentSource,
		GCCPUSeconds: gcCPU, ProcessCPUSeconds: procCPU, GCCPUShare: share,
		Role: role, PID: os.Getpid(),
		LiveHeapBytes: m.HeapAlloc, HeapInUseBytes: m.HeapInuse,
		HeapIdleBytes: m.HeapIdle, HeapReleasedBytes: m.HeapReleased,
		HeapSysBytes: m.HeapSys, StackInUseBytes: m.StackInuse,
		GoSysBytes: m.Sys, NextGCBytes: m.NextGC, NumGC: m.NumGC,
		Goroutines:    runtime.NumGoroutine(),
		ResidentBytes: residentBytes(), MemoryLimitBytes: currentLimit(),
		LimitSource: src, GC: gc,
	}
}

// residentBytes reads the resident set size from /proc/self/statm.
func residentBytes() uint64 {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 0
	}
	pages, err := strconv.ParseUint(f[1], 10, 64)
	if err != nil {
		return 0
	}
	return pages * uint64(os.Getpagesize())
}

// WriteHeapProfile writes a pprof heap profile (after a collection, so
// it shows live objects) to a new file in dir, readable only by the
// user, and returns its path. The caller never chooses the file name.
func WriteHeapProfile(dir, role string) (string, error) {
	if dir == "" {
		dir = os.TempDir()
	}
	name := fmt.Sprintf("xerotty-heap-%s-%d-%d.pprof", role, os.Getpid(), time.Now().UnixNano())
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	runtime.GC()
	if err := pprof.Lookup("heap").WriteTo(f, 0); err != nil {
		f.Close()
		os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return path, nil
}

// ReportWithProfile is Read plus, when profile is true, a heap profile
// written to dir.
func ReportWithProfile(role, dir string, gc, profile bool) (Report, error) {
	var path string
	if profile {
		p, err := WriteHeapProfile(dir, role)
		if err != nil {
			return Report{}, err
		}
		path = p
		gc = true
	}
	r := Read(role, gc)
	r.ProfilePath = path
	return r, nil
}
