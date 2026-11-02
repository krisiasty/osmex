// Package runtimestats renders the exporter's own Go runtime and process metrics: the values rgwt shows in its
// application panel, under the conventional client_golang names, plus CPU time and resident memory from procfs.
package runtimestats

import (
	"io/fs"
	"runtime"
	"runtime/metrics"
	"strconv"
	"strings"
	"time"

	"github.com/krisiasty/osmex/internal/expo"
)

// startedAt approximates the process start time, as rgwt's uptime does; it works on every OS.
var startedAt = time.Now()

// Render adds the runtime metrics to page, read at call time. Process CPU time and resident memory come from the
// procfs files in fsys (os.DirFS("/") in production); where those files are missing or unreadable, as on macOS,
// those two metrics are left out.
func Render(page *expo.Page, fsys fs.FS) {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	page.Gauge("process_start_time_seconds", "Start time of the process since unix epoch in seconds.").
		Add(float64(startedAt.UnixNano()) / 1e9)
	page.Gauge("go_goroutines", "Number of goroutines that currently exist.").Add(float64(runtime.NumGoroutine()))
	if threads, ok := threadCount(); ok {
		page.Gauge("go_threads", "Number of OS threads created.").Add(threads)
	}
	page.Gauge("go_sched_gomaxprocs_threads",
		"The current runtime.GOMAXPROCS setting: the number of OS threads that can run Go code simultaneously.").
		Add(float64(runtime.GOMAXPROCS(0)))
	page.Gauge("go_logical_cpus", "Number of logical CPUs usable by the process.").Add(float64(runtime.NumCPU()))
	page.Gauge("go_memstats_heap_alloc_bytes", "Number of heap bytes allocated and still in use.").
		Add(float64(mem.HeapAlloc))
	page.Gauge("go_memstats_heap_inuse_bytes",
		"Number of heap bytes that are in use (allocated objects plus span space reserved for their size class; objects+unused).").
		Add(float64(mem.HeapInuse))
	page.Gauge("go_memstats_stack_inuse_bytes", "Number of bytes in stack spans currently in use by goroutines.").
		Add(float64(mem.StackInuse))
	page.Gauge("go_memstats_sys_bytes", "Number of bytes obtained from the system.").Add(float64(mem.Sys))
	page.Gauge("go_memstats_heap_objects", "Number of currently allocated objects.").Add(float64(mem.HeapObjects))

	renderProcess(page, fsys)
}

// threadCount reads the number of OS threads the runtime has created, as rgwt does.
func threadCount() (float64, bool) {
	samples := []metrics.Sample{{Name: "/sched/threads/total:threads"}}
	metrics.Read(samples)
	if samples[0].Value.Kind() != metrics.KindUint64 {
		return 0, false
	}
	return float64(samples[0].Value.Uint64()), true
}

// userHZ is the kernel's clock tick rate for /proc/self/stat CPU times, fixed at 100 on Linux.
const userHZ = 100

func renderProcess(page *expo.Page, fsys fs.FS) {
	if stat, err := fs.ReadFile(fsys, "proc/self/stat"); err == nil {
		if utime, stime, ok := parseCPUTicks(string(stat)); ok {
			page.Counter("process_cpu_seconds_total", "Total user and system CPU time spent in seconds.").
				Add(float64(utime+stime) / userHZ)
		}
	}
	if status, err := fs.ReadFile(fsys, "proc/self/status"); err == nil {
		if rss, ok := parseVmRSSBytes(string(status)); ok {
			page.Gauge("process_resident_memory_bytes", "Resident memory size in bytes.").Add(float64(rss))
		}
	}
}

// parseCPUTicks extracts utime and stime (proc(5) fields 14 and 15) from /proc/self/stat. The command name (field 2)
// may contain spaces and parentheses, so fields are counted from after the last ") ".
func parseCPUTicks(stat string) (utime, stime uint64, ok bool) {
	_, after, found := strings.CutLast(stat, ") ")
	if !found {
		return 0, 0, false
	}
	rest := strings.Fields(after) // rest[0] is field 3 (state)
	const fUtime, fStime = 14, 15
	if len(rest) <= fStime-3 {
		return 0, 0, false
	}
	utime, err1 := strconv.ParseUint(rest[fUtime-3], 10, 64)
	stime, err2 := strconv.ParseUint(rest[fStime-3], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return utime, stime, true
}

// parseVmRSSBytes extracts VmRSS from /proc/self/status in bytes; the kernel reports it in KiB.
func parseVmRSSBytes(status string) (uint64, bool) {
	for line := range strings.SplitSeq(status, "\n") {
		rest, ok := strings.CutPrefix(line, "VmRSS:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0, false
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return 0, false
		}
		return kb * 1024, true
	}
	return 0, false
}
