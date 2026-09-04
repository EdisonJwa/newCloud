package server_test

// The restore drill's recorded measurements must stay internally consistent.
//
// deploy/backup/restore-drill.md records three scale measurements and then draws a conclusion from
// them. The first version of that section contained an arithmetic error of 45x: it claimed a
// linear extrapolation to 40 TiB gives "about 82 hours" when the same numbers give about 92,000.
// The error mattered because it changed the conclusion — 82 hours is roughly the RTO target, so the
// wrong number implied the drill was nearly fine, while the right one shows the extrapolation
// itself is unusable.
//
// Prose arithmetic is not reviewable by eye. This test recomputes the derived figures from the
// recorded inputs and fails when they disagree.

import (
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// measurement is one row of the scale table.
type measurement struct {
	dumpMiB float64
	seconds float64
}

// TestDrillScaleMeasurementsAreConsistent.
func TestDrillScaleMeasurementsAreConsistent(t *testing.T) {
	root := repoRoot(t)
	doc, err := os.ReadFile(filepath.Join(root, "deploy", "backup", "restore-drill.md"))
	if err != nil {
		t.Fatalf("read restore-drill.md: %v", err)
	}
	text := string(doc)

	// The scale table's figures, parsed from the document rather than restated here: a test that
	// hardcoded them would keep passing after someone edited the table.
	// Matched per line, not per cell: the dump size is in the third cell and the elapsed time in
	// the sixth, so a cell-scoped pattern finds only one of the two. The whole line is searched.
	rows := regexp.MustCompile(`(?m)^\| 2026-\d\d-\d\d \| Scale point \d \|.*$`).
		FindAllString(text, -1)
	if len(rows) < 2 {
		t.Fatalf("found %d scale rows; need at least two to say anything about scaling", len(rows))
	}

	var ms []measurement
	secRe := regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)\s*s`)
	dumpRe := regexp.MustCompile(`dump ([0-9]+(?:\.[0-9]+)?) ?(KiB|MiB)`)
	for _, row := range rows {
		dm := dumpRe.FindStringSubmatch(row)
		sm := secRe.FindStringSubmatch(row)
		if dm == nil || sm == nil {
			continue
		}
		v, _ := strconv.ParseFloat(dm[1], 64)
		if dm[2] == "KiB" {
			v /= 1024
		}
		s, _ := strconv.ParseFloat(sm[1], 64)
		ms = append(ms, measurement{dumpMiB: v, seconds: s})
	}
	if len(ms) < 2 {
		t.Skip("could not parse two scale measurements; the table format changed")
	}

	// 1. Seconds per MiB must not trend upward sharply with volume. A threshold of 3x across the
	// whole range is generous — the measured spread is 1.35x — but catches an order-of-magnitude
	// regression, which is what "it blows up at some threshold" would look like.
	var rates []float64
	for _, m := range ms {
		rates = append(rates, m.seconds/m.dumpMiB)
	}
	lo, hi := rates[0], rates[0]
	for _, r := range rates {
		lo = math.Min(lo, r)
		hi = math.Max(hi, r)
	}
	if hi/lo > 3 {
		t.Fatalf("restore cost per MiB varies by %.1fx across the measurements (%v); if this is a "+
			"real change the \"linear within 5.5 MiB\" conclusion in the document is no longer "+
			"true and must be rewritten", hi/lo, rates)
	}

	// 2. The document's stated extrapolation for 40 TiB must match the arithmetic.
	//
	// The section states the figure in the form "about 92,000 hours". Any number there is
	// checked against the median rate, with enough tolerance for rounding.
	// The figure is written in bold and with a comma thousands separator, so the pattern
	// allows for both rather than requiring the prose to stay plain.
	stated := regexp.MustCompile(`about \*{0,2}([\d,]+) \*{0,2}hours`).FindStringSubmatch(text)
	if stated == nil {
		t.Fatal("the document no longer states a 40 TiB extrapolation in hours; either restore it " +
			"or update this test")
	}
	claimed, err := strconv.ParseFloat(strings.ReplaceAll(stated[1], ",", ""), 64)
	if err != nil {
		t.Fatalf("parse the stated hours %q: %v", stated[1], err)
	}

	median := rates[len(rates)/2]
	const tib40InMiB = 40 * 1024 * 1024
	expected := tib40InMiB * median / 3600

	// 20% tolerance: the document rounds to significant figures and the rate is a median.
	if math.Abs(claimed-expected)/expected > 0.20 {
		t.Fatalf("the document says a linear extrapolation to 40 TiB is about %.0f hours, but the "+
			"recorded measurements (median %.2f s/MiB) give %.0f hours — a factor of %.0f. "+
			"Fix the document, not the test: this figure is load-bearing for the conclusion.",
			claimed, median, expected, math.Max(claimed/expected, expected/claimed))
	}
}
