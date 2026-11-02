package osclient

import (
	"testing"
	"time"
)

func TestParseTime(t *testing.T) {
	base := time.Date(2026, 10, 7, 15, 3, 39, 0, time.UTC)
	cases := []struct {
		in   string
		want time.Time
	}{
		{"2026-10-07T15:03:39Z", base},
		{"2026-10-07T15:03:39.123456Z", base.Add(123456 * time.Microsecond)},
		{"2026-10-07T17:03:39+02:00", base},
		{"2026-10-07T15:03:39.000000", base},
		{"2026-10-07T15:03:39", base},
		{"2026-10-07T15:03:39.5", base.Add(500 * time.Millisecond)},
	}
	for _, c := range cases {
		got, err := ParseTime(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if !got.Equal(c.want) {
			t.Errorf("%q = %v, want %v", c.in, got, c.want)
		}
	}
	if got, err := ParseTime(""); err != nil || !got.IsZero() {
		t.Errorf(`"" = %v, %v`, got, err)
	}
	if _, err := ParseTime("yesterday"); err == nil {
		t.Error("garbage accepted")
	}
}
