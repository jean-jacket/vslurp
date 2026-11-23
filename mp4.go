package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
)

// box is one MP4 box: its type, where its payload starts and where it ends.
type box struct {
	typ        string
	start, end int64 // payload start (after header) and end, relative to the buffer/file
	offset     int64 // offset of the box header
}

// parseBoxHeader reads a box header from b (at least 16 bytes, or 8 for small boxes).
// fileEnd is used for size==0 ("extends to end of file").
func parseBoxHeader(b []byte, offset, fileEnd int64) (box, error) {
	if len(b) < 8 {
		return box{}, errors.New("short box header")
	}
	size := int64(binary.BigEndian.Uint32(b[0:4]))
	typ := string(b[4:8])
	hdr := int64(8)
	switch size {
	case 1:
		if len(b) < 16 {
			return box{}, errors.New("short 64-bit box header")
		}
		size = int64(binary.BigEndian.Uint64(b[8:16]))
		hdr = 16
	case 0:
		size = fileEnd - offset
	}
	if size < hdr || offset+size > fileEnd {
		return box{}, fmt.Errorf("invalid %q box size %d at offset %d", typ, size, offset)
	}
	return box{typ: typ, offset: offset, start: offset + hdr, end: offset + size}, nil
}

// children lists the boxes directly inside buf[start:end].
func children(buf []byte, start, end int64) ([]box, error) {
	var out []box
	for off := start; off+8 <= end; {
		bx, err := parseBoxHeader(buf[off:end], off, end)
		if err != nil {
			return nil, err
		}
		out = append(out, bx)
		off = bx.end
	}
	return out, nil
}

func findChild(buf []byte, start, end int64, path ...string) (box, bool) {
	cur := box{start: start, end: end}
	for _, name := range path {
		kids, err := children(buf, cur.start, cur.end)
		if err != nil {
			return box{}, false
		}
		found := false
		for _, k := range kids {
			if k.typ == name {
				cur, found = k, true
				break
			}
		}
		if !found {
			return box{}, false
		}
	}
	return cur, true
}

// track holds the sample tables needed to map time to byte offsets.
type track struct {
	trak       box // the trak box within movie.raw
	handler    string
	timescale  uint32
	mediaStart int64 // media time where presentation starts (from edit list)

	sttsCount, sttsDelta []uint32
	cttsCount            []uint32 // composition offsets (pts - dts), run-length encoded; nil if absent
	cttsOffset           []int32
	cttsVersion          byte
	sync                 []uint32 // 1-based sample numbers of keyframes; nil means every sample is a keyframe
	sizes                []uint32
	fixedSize            uint32
	nSamples             int
	stscFirst, stscPer   []uint32
	stscSDI              []uint32 // sample description index per stsc entry
	chunkOffsets         []int64
}

type movie struct {
	tracks    []*track
	raw       []byte // the whole moov box
	kids      []box  // moov's children
	timescale uint32 // from mvhd
}

func u32(b []byte, off int64) uint32 { return binary.BigEndian.Uint32(b[off : off+4]) }
func u64(b []byte, off int64) uint64 { return binary.BigEndian.Uint64(b[off : off+8]) }

// parseMoov parses a moov box payload (buf is the whole moov box including its header).
func parseMoov(buf []byte) (*movie, error) {
	top, err := parseBoxHeader(buf, 0, int64(len(buf)))
	if err != nil {
		return nil, err
	}
	if top.typ != "moov" {
		return nil, fmt.Errorf("expected moov, got %q", top.typ)
	}
	kids, err := children(buf, top.start, top.end)
	if err != nil {
		return nil, err
	}
	m := &movie{raw: buf, kids: kids}
	for _, k := range kids {
		if k.typ == "mvhd" {
			if buf[k.start] == 1 {
				m.timescale = u32(buf, k.start+4+16)
			} else {
				m.timescale = u32(buf, k.start+4+8)
			}
		}
		if k.typ == "mvex" {
			return nil, errors.New("fragmented MP4 (moof/mvex) is not supported")
		}
		if k.typ != "trak" {
			continue
		}
		t, err := parseTrak(buf, k)
		if err != nil {
			return nil, fmt.Errorf("trak at %d: %w", k.offset, err)
		}
		if t != nil {
			m.tracks = append(m.tracks, t)
		}
	}
	if len(m.tracks) == 0 {
		return nil, errors.New("no usable tracks in moov")
	}
	return m, nil
}

func parseTrak(buf []byte, trak box) (*track, error) {
	t := &track{trak: trak}
	if hd, ok := findChild(buf, trak.start, trak.end, "mdia", "hdlr"); ok {
		t.handler = string(buf[hd.start+8 : hd.start+12])
	}
	mdhd, ok := findChild(buf, trak.start, trak.end, "mdia", "mdhd")
	if !ok {
		return nil, errors.New("missing mdhd")
	}
	if buf[mdhd.start] == 1 {
		t.timescale = u32(buf, mdhd.start+4+16)
	} else {
		t.timescale = u32(buf, mdhd.start+4+8)
	}
	if t.timescale == 0 {
		return nil, errors.New("zero timescale")
	}
	if elst, ok := findChild(buf, trak.start, trak.end, "edts", "elst"); ok {
		ver := buf[elst.start]
		n := u32(buf, elst.start+4)
		p := elst.start + 8
		for i := uint32(0); i < n; i++ {
			var mt int64
			if ver == 1 {
				mt = int64(u64(buf, p+8))
				p += 20
			} else {
				mt = int64(int32(u32(buf, p+4)))
				p += 12
			}
			if mt >= 0 { // first non-empty edit
				t.mediaStart = mt
				break
			}
		}
	}

	stbl, ok := findChild(buf, trak.start, trak.end, "mdia", "minf", "stbl")
	if !ok {
		return nil, errors.New("missing stbl")
	}
	tabs, err := children(buf, stbl.start, stbl.end)
	if err != nil {
		return nil, err
	}
	have := map[string]box{}
	for _, b := range tabs {
		have[b.typ] = b
	}

	stts, ok := have["stts"]
	if !ok {
		return nil, errors.New("missing stts")
	}
	n := int64(u32(buf, stts.start+4))
	t.sttsCount, t.sttsDelta = make([]uint32, n), make([]uint32, n)
	for i := int64(0); i < n; i++ {
		t.sttsCount[i] = u32(buf, stts.start+8+i*8)
		t.sttsDelta[i] = u32(buf, stts.start+12+i*8)
	}

	if ctts, ok := have["ctts"]; ok {
		n := int64(u32(buf, ctts.start+4))
		t.cttsVersion = buf[ctts.start]
		t.cttsCount, t.cttsOffset = make([]uint32, n), make([]int32, n)
		for i := int64(0); i < n; i++ {
			t.cttsCount[i] = u32(buf, ctts.start+8+i*8)
			// Version 0 is formally unsigned, but writers only ever store values
			// that fit in int32, so reading both versions as signed is safe.
			t.cttsOffset[i] = int32(u32(buf, ctts.start+12+i*8))
		}
	}

	if stss, ok := have["stss"]; ok {
		n := int64(u32(buf, stss.start+4))
		t.sync = make([]uint32, n)
		for i := int64(0); i < n; i++ {
			t.sync[i] = u32(buf, stss.start+8+i*4)
		}
	}

	stsz, ok := have["stsz"]
	if !ok {
		if _, z2 := have["stz2"]; z2 {
			return nil, errors.New("stz2 sample sizes are not supported")
		}
		return nil, errors.New("missing stsz")
	}
	t.fixedSize = u32(buf, stsz.start+4)
	t.nSamples = int(u32(buf, stsz.start+8))
	if t.fixedSize == 0 {
		t.sizes = make([]uint32, t.nSamples)
		for i := 0; i < t.nSamples; i++ {
			t.sizes[i] = u32(buf, stsz.start+12+int64(i)*4)
		}
	}

	stsc, ok := have["stsc"]
	if !ok {
		return nil, errors.New("missing stsc")
	}
	n = int64(u32(buf, stsc.start+4))
	t.stscFirst, t.stscPer, t.stscSDI = make([]uint32, n), make([]uint32, n), make([]uint32, n)
	for i := int64(0); i < n; i++ {
		t.stscFirst[i] = u32(buf, stsc.start+8+i*12)
		t.stscPer[i] = u32(buf, stsc.start+12+i*12)
		t.stscSDI[i] = u32(buf, stsc.start+16+i*12)
	}

	if co, ok := have["co64"]; ok {
		n := int64(u32(buf, co.start+4))
		t.chunkOffsets = make([]int64, n)
		for i := int64(0); i < n; i++ {
			t.chunkOffsets[i] = int64(u64(buf, co.start+8+i*8))
		}
	} else if co, ok := have["stco"]; ok {
		n := int64(u32(buf, co.start+4))
		t.chunkOffsets = make([]int64, n)
		for i := int64(0); i < n; i++ {
			t.chunkOffsets[i] = int64(u32(buf, co.start+8+i*4))
		}
	} else {
		return nil, errors.New("missing stco/co64")
	}

	if t.nSamples == 0 || len(t.chunkOffsets) == 0 {
		return nil, nil // empty track, ignore
	}
	return t, nil
}

func (t *track) size(i int) int64 {
	if t.fixedSize != 0 {
		return int64(t.fixedSize)
	}
	return int64(t.sizes[i])
}

// sampleAt returns the 0-based index of the last sample whose decode time is <= mediaTime.
func (t *track) sampleAt(mediaTime int64) int {
	if mediaTime <= 0 {
		return 0
	}
	idx, cur := 0, int64(0)
	for i, c := range t.sttsCount {
		d := int64(t.sttsDelta[i])
		span := int64(c) * d
		if mediaTime < cur+span {
			if d == 0 {
				return idx
			}
			return idx + int((mediaTime-cur)/d)
		}
		cur += span
		idx += int(c)
	}
	return t.nSamples - 1
}

// keyframeAtOrBefore returns the 0-based index of the keyframe at or before sample i.
func (t *track) keyframeAtOrBefore(i int) int {
	if t.sync == nil {
		return i
	}
	k := sort.Search(len(t.sync), func(j int) bool { return int(t.sync[j]) > i+1 }) - 1
	if k < 0 {
		return 0
	}
	return int(t.sync[k]) - 1
}

// pts returns the presentation time of sample i in media timescale units
// (decode time plus composition offset, before the edit list is applied).
func (t *track) pts(i int) int64 {
	return t.dts(i) + t.ctts(i)
}

// dts returns the decode time of sample i in media timescale units.
func (t *track) dts(i int) int64 {
	var dts int64
	idx := 0
	for j, c := range t.sttsCount {
		if i < idx+int(c) {
			dts += int64(i-idx) * int64(t.sttsDelta[j])
			break
		}
		dts += int64(c) * int64(t.sttsDelta[j])
		idx += int(c)
	}
	return dts
}

// ctts returns the composition offset (pts - dts) of sample i.
func (t *track) ctts(i int) int64 {
	idx := 0
	for j, c := range t.cttsCount {
		if i < idx+int(c) {
			return int64(t.cttsOffset[j])
		}
		idx += int(c)
	}
	return 0
}

// keyframeForPts returns the index of the last keyframe displayed at or before
// media time target.
func (t *track) keyframeForPts(target int64) int {
	k := t.keyframeAtOrBefore(t.sampleAt(target))
	// With composition offsets a keyframe's display time can be later than its
	// decode time; step back until it is shown at or before the target.
	for k > 0 && t.pts(k) > target {
		k = t.keyframeAtOrBefore(k - 1)
	}
	return k
}

// keyframeStart returns the presentation time, in timescale units relative to
// the start of the video, of the last keyframe shown at or before sec.
func (t *track) keyframeStart(sec float64) int64 {
	k := t.keyframeForPts(int64(sec*float64(t.timescale)) + t.mediaStart)
	return max(t.pts(k)-t.mediaStart, 0)
}

// snapToKeyframe returns the display time (seconds) of the video keyframe at or
// before sec, so a stream copy can start exactly on it. ok is false if the file
// has no video track with a keyframe table.
func (m *movie) snapToKeyframe(sec float64) (snapped float64, ok bool) {
	for _, t := range m.tracks {
		if t.handler != "vide" || t.sync == nil {
			continue
		}
		return float64(t.keyframeStart(sec)) / float64(t.timescale), true
	}
	return sec, false
}

// offset returns the file offset of sample i.
func (t *track) offset(i int) (int64, error) {
	base := 0
	for j := range t.stscFirst {
		first := int(t.stscFirst[j])
		next := len(t.chunkOffsets) + 1
		if j+1 < len(t.stscFirst) {
			next = int(t.stscFirst[j+1])
		}
		per := int(t.stscPer[j])
		span := (next - first) * per
		if i < base+span {
			chunk := first - 1 + (i-base)/per
			if chunk >= len(t.chunkOffsets) {
				break
			}
			firstInChunk := i - (i-base)%per
			off := t.chunkOffsets[chunk]
			for s := firstInChunk; s < i; s++ {
				off += t.size(s)
			}
			return off, nil
		}
		base += span
	}
	return 0, fmt.Errorf("sample %d out of range", i)
}

// byteRange returns the [lo, hi) file byte range that covers every track's samples
// needed for presentation times [startSec, endSec]. endSec < 0 means "to the end".
// pad widens the time window on both sides to cover reordering and edit-list slop.
func (m *movie) byteRange(startSec, endSec, pad float64) (lo, hi int64, err error) {
	lo, hi = -1, -1
	for _, t := range m.tracks {
		ts := float64(t.timescale)
		s := 0
		if startSec > 0 {
			s = t.keyframeAtOrBefore(t.sampleAt(int64((startSec-pad)*ts) + t.mediaStart))
		}
		e := t.nSamples - 1
		if endSec >= 0 {
			e = t.sampleAt(int64((endSec+pad)*ts) + t.mediaStart)
		}
		so, err := t.offset(s)
		if err != nil {
			return 0, 0, err
		}
		eo, err := t.offset(e)
		if err != nil {
			return 0, 0, err
		}
		eo += t.size(e)
		if lo < 0 || so < lo {
			lo = so
		}
		if eo > hi {
			hi = eo
		}
	}
	return lo, hi, nil
}

// duration in seconds of the longest track.
func (m *movie) duration() float64 {
	best := 0.0
	for _, t := range m.tracks {
		var total int64
		for i, c := range t.sttsCount {
			total += int64(c) * int64(t.sttsDelta[i])
		}
		if d := float64(total-t.mediaStart) / float64(t.timescale); d > best {
			best = d
		}
	}
	return best
}
