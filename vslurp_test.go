package main

import (
	"encoding/binary"
	"reflect"
	"testing"
)

func TestParseRange(t *testing.T) {
	cases := []struct {
		in         string
		start, end float64
		bad        bool
	}{
		{"01:00:00-02:00:00", 3600, 7200, false},
		{"01:00:00-", 3600, -1, false},
		{"-01:00:00", -1, 3600, false},
		{"90", 90, -1, false},
		{"1:30.5-2:00", 90.5, 120, false},
		{"-", 0, 0, true},
		{"", 0, 0, true},
		{"02:00:00-01:00:00", 0, 0, true},
		{"1:2:3:4-", 0, 0, true},
		{"abc-", 0, 0, true},
		{"1.5:00-", 0, 0, true},
	}
	for _, c := range cases {
		r, err := parseRange(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("parseRange(%q): expected error", c.in)
			}
			continue
		}
		if err != nil || r.start != c.start || r.end != c.end {
			t.Errorf("parseRange(%q) = %v,%v,%v; want %v,%v", c.in, r.start, r.end, err, c.start, c.end)
		}
	}
}

func TestSplitArgs(t *testing.T) {
	flags, pos, err := splitArgs([]string{"-01:00:00", "-j", "8", "http://x/a.mp4", "--user-agent=foo", "out.mp4"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(flags, []string{"-j", "8", "--user-agent=foo"}) {
		t.Errorf("flags = %q", flags)
	}
	if !reflect.DeepEqual(pos, []string{"-01:00:00", "http://x/a.mp4", "out.mp4"}) {
		t.Errorf("pos = %q", pos)
	}
	if _, _, err := splitArgs([]string{"-bogus", "a"}); err == nil {
		t.Error("expected error for unknown flag")
	}
}

func TestSpans(t *testing.T) {
	var s spans
	s = s.add(span{10, 20})
	s = s.add(span{30, 40})
	s = s.add(span{20, 25}) // touches the first: merges
	if !reflect.DeepEqual(s, spans{{10, 25}, {30, 40}}) {
		t.Fatalf("add: %v", s)
	}
	got := s.missing(span{0, 50})
	if !reflect.DeepEqual(got, spans{{0, 10}, {25, 30}, {40, 50}}) {
		t.Errorf("missing: %v", got)
	}
	if m := s.missing(span{12, 24}); len(m) != 0 {
		t.Errorf("missing inside: %v", m)
	}
	s = s.add(span{0, 100})
	if !reflect.DeepEqual(s, spans{{0, 100}}) {
		t.Errorf("cover: %v", s)
	}
}

// --- synthetic MP4 index ---

func mkbox(typ string, payload ...[]byte) []byte {
	n := 8
	for _, p := range payload {
		n += len(p)
	}
	b := make([]byte, 8, n)
	binary.BigEndian.PutUint32(b, uint32(n))
	copy(b[4:], typ)
	for _, p := range payload {
		b = append(b, p...)
	}
	return b
}

func be32(vs ...uint32) []byte {
	b := make([]byte, 4*len(vs))
	for i, v := range vs {
		binary.BigEndian.PutUint32(b[i*4:], v)
	}
	return b
}

// One track: 10 samples, 1 s each (timescale 1000), 100 bytes each,
// 2 samples per chunk, chunks at 1000, 2000, ..., keyframes at samples 1, 5, 9.
func testMoov() []byte {
	mdhd := mkbox("mdhd", be32(0, 0, 0, 1000, 10000, 0))
	stts := mkbox("stts", be32(0, 1, 10, 1000))
	stss := mkbox("stss", be32(0, 3, 1, 5, 9))
	sizes := []uint32{0, 0, 10}
	for i := 0; i < 10; i++ {
		sizes = append(sizes, 100)
	}
	stsz := mkbox("stsz", be32(sizes...))
	stsc := mkbox("stsc", be32(0, 1, 1, 2, 1))
	stco := mkbox("stco", be32(0, 5, 1000, 2000, 3000, 4000, 5000))
	stbl := mkbox("stbl", stts, stss, stsz, stsc, stco)
	trak := mkbox("trak", mkbox("mdia", mdhd, mkbox("minf", stbl)))
	return mkbox("moov", trak)
}

func TestByteRange(t *testing.T) {
	m, err := parseMoov(testMoov())
	if err != nil {
		t.Fatal(err)
	}
	if d := m.duration(); d != 10 {
		t.Errorf("duration = %v", d)
	}
	cases := []struct {
		start, end float64
		lo, hi     int64
	}{
		// 6.5 s is sample 7 (index 6) -> back to keyframe sample 5 (index 4, chunk 3 @3000).
		// 7.5 s is index 7: chunk 4 @4000, second in chunk -> 4100..4200.
		{6.5, 7.5, 3000, 4200},
		{-1, 2.5, 1000, 2100}, // from the start
		{8.2, -1, 5000, 5200}, // index 8 is keyframe sample 9, chunk 5 @5000; to the end
		{0, -1, 1000, 5200},   // whole file
	}
	for _, c := range cases {
		lo, hi, err := m.byteRange(c.start, c.end, 0)
		if err != nil || lo != c.lo || hi != c.hi {
			t.Errorf("byteRange(%v,%v) = %d,%d,%v; want %d,%d", c.start, c.end, lo, hi, err, c.lo, c.hi)
		}
	}
}

func TestParseContentRange(t *testing.T) {
	s, e, tot, ok := parseContentRange("bytes 0-15/66187559384")
	if !ok || s != 0 || e != 15 || tot != 66187559384 {
		t.Errorf("got %d %d %d %v", s, e, tot, ok)
	}
	if _, _, _, ok := parseContentRange("bytes */100"); ok {
		t.Error("expected failure")
	}
}

// testMoovVideo is testMoov with a "vide" handler and an optional ctts box.
func testMoovVideo(ctts []byte) []byte {
	hdlr := mkbox("hdlr", be32(0, 0), []byte("vide"), be32(0, 0, 0), []byte{0})
	mdhd := mkbox("mdhd", be32(0, 0, 0, 1000, 10000, 0))
	stts := mkbox("stts", be32(0, 1, 10, 1000))
	stss := mkbox("stss", be32(0, 3, 1, 5, 9))
	sizes := []uint32{0, 0, 10}
	for i := 0; i < 10; i++ {
		sizes = append(sizes, 100)
	}
	stsz := mkbox("stsz", be32(sizes...))
	stsc := mkbox("stsc", be32(0, 1, 1, 2, 1))
	stco := mkbox("stco", be32(0, 5, 1000, 2000, 3000, 4000, 5000))
	tabs := [][]byte{mkbox("stsd", be32(0, 0)), stts}
	if ctts != nil {
		tabs = append(tabs, ctts)
	}
	tabs = append(tabs, stss, stsz, stsc, stco)
	stbl := mkbox("stbl", tabs...)
	trak := mkbox("trak", mkbox("mdia", mdhd, hdlr, mkbox("minf", stbl)))
	return mkbox("moov", trak)
}

func TestSnapToKeyframe(t *testing.T) {
	m, err := parseMoov(testMoovVideo(nil))
	if err != nil {
		t.Fatal(err)
	}
	// Keyframes at 0 s, 4 s, 8 s.
	for _, c := range [][2]float64{{6.5, 4}, {4, 4}, {3.99, 0}, {8, 8}, {9.9, 8}, {0.5, 0}} {
		got, ok := m.snapToKeyframe(c[0])
		if !ok || got != c[1] {
			t.Errorf("snapToKeyframe(%v) = %v,%v; want %v", c[0], got, ok, c[1])
		}
	}

	// Every sample shown 0.5 s after it is decoded, except keyframe sample 5,
	// shown 1.5 s after (at 5.5 s). Asking for 5.0 s must fall back to keyframe 1.
	ctts := mkbox("ctts", be32(0, 3, 4, 500, 1, 1500, 5, 500))
	m, err = parseMoov(testMoovVideo(ctts))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range [][2]float64{{5.0, 0.5}, {5.5, 5.5}, {7, 5.5}, {8.6, 8.5}} {
		got, _ := m.snapToKeyframe(c[0])
		if got != c[1] {
			t.Errorf("with ctts: snapToKeyframe(%v) = %v; want %v", c[0], got, c[1])
		}
	}
}

func TestFmtFFTime(t *testing.T) {
	for _, c := range []struct {
		in   float64
		want string
	}{
		{21597.449, "05:59:57.449000"},
		{0, "00:00:00.000000"},
		{1.0000001, "00:00:01.000001"}, // rounds up, never down
		{3600.5, "01:00:00.500000"},
	} {
		if got := fmtFFTime(c.in); got != c.want {
			t.Errorf("fmtFFTime(%v) = %q; want %q", c.in, got, c.want)
		}
	}
}

// testMoovFull is testMoovVideo with mvhd and tkhd, so it can be cut.
func testMoovFull() []byte {
	inner := testMoovVideo(mkbox("ctts", be32(0, 1, 10, 0)))
	moov, _ := parseBoxHeader(inner, 0, int64(len(inner)))
	trak := inner[moov.start:moov.end] // the single trak box
	mvhd := mkbox("mvhd", be32(0, 0, 0, 1000, 10000), make([]byte, 80))
	tb, _ := parseBoxHeader(trak, 0, int64(len(trak)))
	tkhd := mkbox("tkhd", be32(0, 0, 0, 1, 0, 10000), make([]byte, 60))
	newTrak := mkbox("trak", tkhd, trak[tb.start:tb.end])
	return mkbox("moov", mvhd, newTrak)
}

func TestPlanCut(t *testing.T) {
	src, err := parseMoov(testMoovFull())
	if err != nil {
		t.Fatal(err)
	}
	ftyp := mkbox("ftyp", []byte("isom"), be32(0))
	// Keyframes at 0, 4, 8 s. Cut 4 s .. 7.5 s: samples 4..7 (index), 3.5 s presented.
	plan, err := src.planCut(ftyp, 4, 7.5)
	if err != nil {
		t.Fatal(err)
	}
	if plan.lo != 3000 || plan.hi != 4200 {
		t.Errorf("source range = %d..%d; want 3000..4200", plan.lo, plan.hi)
	}

	// Parse the header back: ftyp, moov, then the 16-byte mdat header.
	h := plan.header
	mb, err := parseBoxHeader(h[len(ftyp):], int64(len(ftyp)), int64(len(h)))
	if err != nil || mb.typ != "moov" {
		t.Fatalf("moov: %v %q", err, mb.typ)
	}
	out, err := parseMoov(h[mb.offset:mb.end])
	if err != nil {
		t.Fatal(err)
	}
	base := mb.end + 16
	if string(h[mb.end+4:mb.end+8]) != "mdat" || int64(len(h)) != base {
		t.Fatal("bad mdat header")
	}
	if got := int64(binary.BigEndian.Uint64(h[mb.end+8:])); got != 16+plan.hi-plan.lo {
		t.Errorf("mdat size %d", got)
	}

	tr := out.tracks[0]
	if tr.nSamples != 4 {
		t.Fatalf("samples = %d; want 4", tr.nSamples)
	}
	if !reflect.DeepEqual(tr.sync, []uint32{1}) {
		t.Errorf("keyframes = %v; want [1]", tr.sync)
	}
	orig := src.tracks[0]
	for j := 0; j < 4; j++ {
		got, _ := tr.offset(j)
		want, _ := orig.offset(4 + j)
		if got-base+plan.lo != want {
			t.Errorf("sample %d maps to source %d; want %d", j, got-base+plan.lo, want)
		}
	}
	if d := out.duration(); d != 4 { // media duration of the 4 samples
		t.Errorf("media duration = %v", d)
	}

	// Presented duration in mvhd (v0: payload offset 16) is 3.5 s.
	mv := out.kids[0]
	if mv.typ != "mvhd" || binary.BigEndian.Uint32(out.raw[mv.start+16:]) != 3500 {
		t.Errorf("mvhd duration wrong")
	}
	// Edit list starts at media time 0 (keyframe, no composition offset) for 3.5 s.
	if tr.mediaStart != 0 {
		t.Errorf("edit list media time = %d", tr.mediaStart)
	}

	// Open-ended: from 8 s to the end is samples 8..9.
	plan, err = src.planCut(ftyp, 8, -1)
	if err != nil {
		t.Fatal(err)
	}
	if plan.lo != 5000 || plan.hi != 5200 {
		t.Errorf("open-ended range = %d..%d", plan.lo, plan.hi)
	}
}
