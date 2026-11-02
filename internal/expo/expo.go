// Package expo renders metrics in the Prometheus text exposition format, version 0.0.4.
package expo

import (
	"bytes"
	"math"
	"slices"
	"strconv"
	"strings"
)

// Page is an ordered set of metric families. The zero value is ready to use.
type Page struct {
	families []*Family
}

// Family is one metric family: a name, help text, type and its samples.
type Family struct {
	name    string
	help    string
	typ     string
	samples []sample
}

type sample struct {
	labels string // rendered label set including braces, or "" for none
	value  float64
}

// Gauge adds a gauge family to the page.
func (p *Page) Gauge(name, help string) *Family { return p.add(name, help, "gauge") }

// Counter adds a counter family to the page.
func (p *Page) Counter(name, help string) *Family { return p.add(name, help, "counter") }

func (p *Page) add(name, help, typ string) *Family {
	f := &Family{name: name, help: help, typ: typ}
	p.families = append(p.families, f)
	return f
}

// Add appends a sample. labels are name/value pairs; an odd count panics.
func (f *Family) Add(value float64, labels ...string) {
	if len(labels)%2 != 0 {
		panic("expo: odd number of label arguments for " + f.name)
	}
	f.samples = append(f.samples, sample{labels: renderLabels(labels), value: value})
}

// Bytes renders the page. Families appear in creation order, families without samples are omitted,
// and samples within a family are sorted by their rendered label set.
func (p *Page) Bytes() []byte {
	var b bytes.Buffer
	for _, f := range p.families {
		if len(f.samples) == 0 {
			continue
		}
		slices.SortStableFunc(f.samples, func(a, b sample) int { return strings.Compare(a.labels, b.labels) })
		b.WriteString("# HELP ")
		b.WriteString(f.name)
		b.WriteByte(' ')
		b.WriteString(helpEscaper.Replace(f.help))
		b.WriteString("\n# TYPE ")
		b.WriteString(f.name)
		b.WriteByte(' ')
		b.WriteString(f.typ)
		b.WriteByte('\n')
		for _, s := range f.samples {
			b.WriteString(f.name)
			b.WriteString(s.labels)
			b.WriteByte(' ')
			b.WriteString(formatFloat(s.value))
			b.WriteByte('\n')
		}
	}
	return b.Bytes()
}

// Bool returns 1 for true and 0 for false.
func Bool(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

var (
	labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
)

func renderLabels(pairs []string) string {
	if len(pairs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i := 0; i < len(pairs); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(pairs[i])
		b.WriteString(`="`)
		b.WriteString(labelEscaper.Replace(pairs[i+1]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
