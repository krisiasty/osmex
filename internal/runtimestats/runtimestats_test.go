package runtimestats

import (
	"runtime"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/krisiasty/osmex/internal/expo"
)

func render(t *testing.T, fsys fstest.MapFS) string {
	t.Helper()
	var page expo.Page
	Render(&page, fsys)
	return string(page.Bytes())
}

// value returns the value of an unlabelled sample.
func value(t *testing.T, body, name string) float64 {
	t.Helper()
	for line := range strings.SplitSeq(body, "\n") {
		if rest, ok := strings.CutPrefix(line, name+" "); ok {
			v, err := strconv.ParseFloat(rest, 64)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			return v
		}
	}
	t.Fatalf("%s missing from:\n%s", name, body)
	return 0
}

func TestRuntimeMetrics(t *testing.T) {
	body := render(t, fstest.MapFS{})
	var families []string
	for line := range strings.SplitSeq(body, "\n") {
		if rest, ok := strings.CutPrefix(line, "# TYPE "); ok {
			families = append(families, rest)
		}
	}
	want := []string{
		"process_start_time_seconds gauge",
		"go_goroutines gauge",
		"go_threads gauge",
		"go_sched_gomaxprocs_threads gauge",
		"go_logical_cpus gauge",
		"go_memstats_heap_alloc_bytes gauge",
		"go_memstats_heap_inuse_bytes gauge",
		"go_memstats_stack_inuse_bytes gauge",
		"go_memstats_sys_bytes gauge",
		"go_memstats_heap_objects gauge",
	}
	if strings.Join(families, "\n") != strings.Join(want, "\n") {
		t.Errorf("families:\n%s\nwant:\n%s", strings.Join(families, "\n"), strings.Join(want, "\n"))
	}

	if v := value(t, body, "go_logical_cpus"); v != float64(runtime.NumCPU()) {
		t.Errorf("go_logical_cpus = %g, want %d", v, runtime.NumCPU())
	}
	if v := value(t, body, "go_sched_gomaxprocs_threads"); v != float64(runtime.GOMAXPROCS(0)) {
		t.Errorf("go_sched_gomaxprocs_threads = %g, want %d", v, runtime.GOMAXPROCS(0))
	}
	for _, name := range []string{"go_goroutines", "go_threads", "go_memstats_heap_alloc_bytes", "go_memstats_heap_objects",
		"go_memstats_stack_inuse_bytes", "go_memstats_sys_bytes"} {
		if v := value(t, body, name); v <= 0 {
			t.Errorf("%s = %g, want > 0", name, v)
		}
	}
	if alloc, inuse := value(t, body, "go_memstats_heap_alloc_bytes"), value(t, body, "go_memstats_heap_inuse_bytes"); inuse < alloc {
		t.Errorf("heap in use %g < heap allocated %g", inuse, alloc)
	}
	start := value(t, body, "process_start_time_seconds")
	if age := time.Since(time.Unix(0, int64(start*1e9))); age < 0 || age > time.Hour {
		t.Errorf("process_start_time_seconds is %v ago", age)
	}
}

func TestProcessReadsProcfs(t *testing.T) {
	// utime 100 + stime 200 ticks at 100 Hz = 3 seconds. The command name contains ") " to check
	// that fields are counted from the last closing parenthesis.
	stat := "1 (osmex) x) S 0 1 1 0 -1 4194304 100 0 0 0 100 200 0 0 20 0 8 0 900 " +
		"1073741824 2048 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 1 0 0 0 0 0\n"
	body := render(t, fstest.MapFS{
		"proc/self/stat":   {Data: []byte(stat)},
		"proc/self/status": {Data: []byte("Name:\tosmex\nVmRSS:\t   8192 kB\nVmSwap:\t0 kB\n")},
	})
	if v := value(t, body, "process_cpu_seconds_total"); v != 3 {
		t.Errorf("process_cpu_seconds_total = %g, want 3", v)
	}
	if !strings.Contains(body, "# TYPE process_cpu_seconds_total counter") {
		t.Error("process_cpu_seconds_total is not a counter")
	}
	if v := value(t, body, "process_resident_memory_bytes"); v != 8192*1024 {
		t.Errorf("process_resident_memory_bytes = %g, want %d", v, 8192*1024)
	}
}

func TestProcessMissingFilesEmitsNothing(t *testing.T) {
	body := render(t, fstest.MapFS{})
	for _, unwanted := range []string{"process_cpu_seconds_total", "process_resident_memory_bytes"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("%s emitted without procfs:\n%s", unwanted, body)
		}
	}
}

func TestProcessMalformedFilesEmitNothing(t *testing.T) {
	body := render(t, fstest.MapFS{
		"proc/self/stat":   {Data: []byte("1 (osmex) S 0 1\n")},
		"proc/self/status": {Data: []byte("VmRSS:\tlots kB\n")},
	})
	for _, unwanted := range []string{"process_cpu_seconds_total", "process_resident_memory_bytes"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("%s emitted from malformed procfs:\n%s", unwanted, body)
		}
	}
}
