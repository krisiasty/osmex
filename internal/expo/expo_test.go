package expo

import (
	"math"
	"testing"
)

func TestPageRendersFamilies(t *testing.T) {
	var p Page
	f := p.Gauge("x_y", "Help.")
	f.Add(2, "id", "b")
	f.Add(1, "id", "a")
	want := "# HELP x_y Help.\n# TYPE x_y gauge\nx_y{id=\"a\"} 1\nx_y{id=\"b\"} 2\n"
	if got := string(p.Bytes()); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestFamiliesInCreationOrder(t *testing.T) {
	var p Page
	p.Gauge("b", "B.").Add(1)
	p.Gauge("a", "A.").Add(1)
	want := "# HELP b B.\n# TYPE b gauge\nb 1\n# HELP a A.\n# TYPE a gauge\na 1\n"
	if got := string(p.Bytes()); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestEmptyFamilyOmitted(t *testing.T) {
	var p Page
	p.Gauge("empty", "Nothing.")
	if got := p.Bytes(); len(got) != 0 {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestCounterType(t *testing.T) {
	var p Page
	p.Counter("c_total", "Count.").Add(3, "k", "v")
	want := "# HELP c_total Count.\n# TYPE c_total counter\nc_total{k=\"v\"} 3\n"
	if got := string(p.Bytes()); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestLabelEscaping(t *testing.T) {
	var p Page
	p.Gauge("m", "M.").Add(1, "name", "a\"b\\c\nd", "u", "zażółć")
	want := "# HELP m M.\n# TYPE m gauge\nm{name=\"a\\\"b\\\\c\\nd\",u=\"zażółć\"} 1\n"
	if got := string(p.Bytes()); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestHelpEscaping(t *testing.T) {
	var p Page
	p.Gauge("m", "a\\b\nc").Add(1)
	want := "# HELP m a\\\\b\\nc\n# TYPE m gauge\nm 1\n"
	if got := string(p.Bytes()); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestFloatFormatting(t *testing.T) {
	cases := map[float64]string{
		1e6:          "1e+06",
		0.5:          "0.5",
		1700000000:   "1.7e+09",
		42:           "42",
		math.Inf(1):  "+Inf",
		math.Inf(-1): "-Inf",
		math.NaN():   "NaN",
	}
	for v, want := range cases {
		if got := formatFloat(v); got != want {
			t.Errorf("formatFloat(%v) = %q, want %q", v, got, want)
		}
	}
}

func TestNoLabels(t *testing.T) {
	var p Page
	p.Gauge("m", "M.").Add(7)
	want := "# HELP m M.\n# TYPE m gauge\nm 7\n"
	if got := string(p.Bytes()); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestOddLabelsPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic")
		}
	}()
	var p Page
	p.Gauge("m", "M.").Add(1, "only-name") //nolint:staticcheck // SA5012: the odd label count is what this test exercises
}

func TestBool(t *testing.T) {
	if Bool(true) != 1 || Bool(false) != 0 {
		t.Fatal("Bool")
	}
}
