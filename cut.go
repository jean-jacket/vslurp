package main

// Building a "fast-start" MP4 holding just the cut: ftyp, a new moov describing
// only the cut's samples, then an mdat whose payload is a verbatim copy of the
// source bytes [lo, hi). Because the index comes first and the media bytes are
// in source order, the file can be played while it is still being written.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

type cutPlan struct {
	header []byte // ftyp + moov + mdat header
	lo, hi int64  // source byte range that forms the mdat payload
}

type trackCut struct {
	t           *track
	first, last int     // inclusive sample range
	offsets     []int64 // source file offset of each sample in the range
	mediaTime   int64   // where presentation starts, relative to the cut's first sample (track timescale)
	mediaDur    int64   // sum of sample durations in the range (track timescale)
	presDur     uint64  // presented duration (movie timescale)
}

// planCut lays out the cut covering presentation times [startSec, endSec].
// startSec must already be on a video keyframe (see snapToKeyframe); endSec < 0
// means to the end of the video.
func (m *movie) planCut(ftyp []byte, startSec, endSec float64) (*cutPlan, error) {
	if m.timescale == 0 {
		return nil, errors.New("missing mvhd timescale")
	}
	var cuts []*trackCut
	lo, hi := int64(-1), int64(-1)
	for _, t := range m.tracks {
		c, err := cutTrack(t, startSec, endSec, m.timescale)
		if err != nil {
			return nil, fmt.Errorf("%s track: %w", t.handlerName(), err)
		}
		if c == nil {
			continue // track has no samples in the range
		}
		cuts = append(cuts, c)
		if a := c.offsets[0]; lo < 0 || a < lo {
			lo = a
		}
		if b := c.offsets[len(c.offsets)-1] + t.size(c.last); b > hi {
			hi = b
		}
	}
	if len(cuts) == 0 {
		return nil, errors.New("the time range contains no samples")
	}

	// The moov's size doesn't depend on the data offset (co64 entries are fixed
	// width), so build it once to measure it, then again with the real offset.
	const mdatHdr = 16
	moov, err := m.buildMoov(cuts, lo, 0)
	if err != nil {
		return nil, err
	}
	base := int64(len(ftyp)+len(moov)) + mdatHdr
	moov, err = m.buildMoov(cuts, lo, base)
	if err != nil {
		return nil, err
	}

	h := make([]byte, 0, len(ftyp)+len(moov)+mdatHdr)
	h = append(h, ftyp...)
	h = append(h, moov...)
	h = binary.BigEndian.AppendUint32(h, 1) // 64-bit size follows
	h = append(h, "mdat"...)
	h = binary.BigEndian.AppendUint64(h, uint64(mdatHdr+hi-lo))
	return &cutPlan{header: h, lo: lo, hi: hi}, nil
}

func (t *track) handlerName() string {
	switch t.handler {
	case "vide":
		return "video"
	case "soun":
		return "audio"
	case "":
		return "unknown"
	}
	return t.handler
}

func cutTrack(t *track, startSec, endSec float64, movieTS uint32) (*trackCut, error) {
	ts := float64(t.timescale)
	var total int64 // media duration of the whole track
	for i, c := range t.sttsCount {
		total += int64(c) * int64(t.sttsDelta[i])
	}

	// Presentation time p maps to media time p*ts + mediaStart.
	start := int64(math.Round(startSec*ts)) + t.mediaStart
	if start >= total {
		return nil, nil
	}
	var first int
	if t.handler == "vide" && t.sync != nil {
		first = t.keyframeForPts(start)
	} else {
		first = t.sampleAt(start)
	}

	last := t.nSamples - 1
	end := total
	if endSec >= 0 {
		end = min(int64(math.Round(endSec*ts))+t.mediaStart, total)
		// Every sample decoded before the end time...
		last = t.sampleAt(end - 1)
		// ...plus any decoded later but displayed before it (B-frame reordering).
		for last+1 < t.nSamples && t.pts(last+1) < end {
			last++
		}
	}
	if last < first {
		return nil, nil
	}

	c := &trackCut{t: t, first: first, last: last}
	firstDTS := t.dts(first)
	c.mediaTime = start - firstDTS
	if c.mediaTime < 0 {
		c.mediaTime = 0
	}
	c.mediaDur = t.dts(last) + int64(t.deltaOf(last)) - firstDTS
	c.presDur = uint64(math.Round(float64(end-start) / ts * float64(movieTS)))

	var err error
	if c.offsets, err = t.offsetsRange(first, last); err != nil {
		return nil, err
	}
	return c, nil
}

// deltaOf returns the duration of sample i.
func (t *track) deltaOf(i int) uint32 {
	j, _ := runAt(t.sttsCount, i)
	if j >= len(t.sttsDelta) {
		return 0
	}
	return t.sttsDelta[j]
}

// runAt finds sample i in a run-length table: the entry index and how far into it.
func runAt(counts []uint32, i int) (entry, within int) {
	for j, c := range counts {
		if i < int(c) {
			return j, i
		}
		i -= int(c)
	}
	return len(counts), 0
}

// offsetsRange returns the file offsets of samples first..last, and checks they
// all use the same sample description.
func (t *track) offsetsRange(first, last int) ([]int64, error) {
	// Find the chunk holding `first` and its position in that chunk.
	j, base := 0, 0
	chunk, pos := -1, 0
	for ; j < len(t.stscFirst); j++ {
		next := len(t.chunkOffsets) + 1
		if j+1 < len(t.stscFirst) {
			next = int(t.stscFirst[j+1])
		}
		per := int(t.stscPer[j])
		span := (next - int(t.stscFirst[j])) * per
		if first < base+span {
			chunk = int(t.stscFirst[j]) - 1 + (first-base)/per
			pos = (first - base) % per
			break
		}
		base += span
	}
	if chunk < 0 || chunk >= len(t.chunkOffsets) {
		return nil, fmt.Errorf("sample %d out of range", first)
	}
	sdi := t.stscSDI[j]

	out := make([]int64, 0, last-first+1)
	off := t.chunkOffsets[chunk]
	for s := first - pos; s < first; s++ {
		off += t.size(s)
	}
	for s := first; s <= last; s++ {
		out = append(out, off)
		off += t.size(s)
		pos++
		if pos == int(t.stscPer[j]) && s < last {
			chunk++
			pos = 0
			if chunk >= len(t.chunkOffsets) {
				return nil, fmt.Errorf("sample %d past the last chunk", s+1)
			}
			if j+1 < len(t.stscFirst) && chunk+1 >= int(t.stscFirst[j+1]) {
				j++
				if t.stscSDI[j] != sdi {
					return nil, errors.New("the codec settings change within the range; use vslurp without -stream")
				}
			}
			off = t.chunkOffsets[chunk]
		}
	}
	return out, nil
}

// ---------- moov serialisation ----------

func appendBox(dst []byte, typ string, payload ...[]byte) []byte {
	n := 8
	for _, p := range payload {
		n += len(p)
	}
	dst = binary.BigEndian.AppendUint32(dst, uint32(n))
	dst = append(dst, typ...)
	for _, p := range payload {
		dst = append(dst, p...)
	}
	return dst
}

func boxBytes(raw []byte, b box) []byte { return raw[b.offset:b.end] }

// withDuration returns a copy of an mvhd/tkhd/mdhd box with its duration replaced.
// v0Off/v1Off are the duration's offsets within the payload for each version.
func withDuration(raw []byte, b box, v0Off, v1Off int64, dur uint64) []byte {
	out := append([]byte(nil), boxBytes(raw, b)...)
	p := b.start - b.offset // payload offset within out
	if out[p] == 1 {
		binary.BigEndian.PutUint64(out[p+v1Off:], dur)
	} else {
		binary.BigEndian.PutUint32(out[p+v0Off:], uint32(min(dur, math.MaxUint32)))
	}
	return out
}

func (m *movie) buildMoov(cuts []*trackCut, lo, base int64) ([]byte, error) {
	byTrak := map[int64]*trackCut{}
	var movieDur uint64
	for _, c := range cuts {
		byTrak[c.t.trak.offset] = c
		movieDur = max(movieDur, c.presDur)
	}
	var body []byte
	for _, k := range m.kids {
		switch k.typ {
		case "mvhd":
			body = append(body, withDuration(m.raw, k, 16, 24, movieDur)...)
		case "trak":
			c, ok := byTrak[k.offset]
			if !ok {
				continue // empty or unused track: leave it out
			}
			trak, err := m.buildTrak(c, lo, base)
			if err != nil {
				return nil, err
			}
			body = append(body, trak...)
		default:
			body = append(body, boxBytes(m.raw, k)...)
		}
	}
	return appendBox(nil, "moov", body), nil
}

func (m *movie) buildTrak(c *trackCut, lo, base int64) ([]byte, error) {
	kids, err := children(m.raw, c.t.trak.start, c.t.trak.end)
	if err != nil {
		return nil, err
	}
	var body []byte
	for _, k := range kids {
		switch k.typ {
		case "tkhd":
			body = append(body, withDuration(m.raw, k, 20, 28, c.presDur)...)
			// A single edit: present from mediaTime for presDur.
			elst := []byte{1, 0, 0, 0} // version 1, flags 0
			elst = binary.BigEndian.AppendUint32(elst, 1)
			elst = binary.BigEndian.AppendUint64(elst, c.presDur)
			elst = binary.BigEndian.AppendUint64(elst, uint64(c.mediaTime))
			elst = binary.BigEndian.AppendUint32(elst, 0x00010000) // rate 1.0
			body = appendBox(body, "edts", appendBox(nil, "elst", elst))
		case "edts":
			// replaced above
		case "mdia":
			mdia, err := m.buildMdia(c, k, lo, base)
			if err != nil {
				return nil, err
			}
			body = append(body, mdia...)
		default:
			body = append(body, boxBytes(m.raw, k)...)
		}
	}
	return appendBox(nil, "trak", body), nil
}

func (m *movie) buildMdia(c *trackCut, mdia box, lo, base int64) ([]byte, error) {
	kids, err := children(m.raw, mdia.start, mdia.end)
	if err != nil {
		return nil, err
	}
	var body []byte
	for _, k := range kids {
		switch k.typ {
		case "mdhd":
			body = append(body, withDuration(m.raw, k, 16, 24, uint64(c.mediaDur))...)
		case "minf":
			minfKids, err := children(m.raw, k.start, k.end)
			if err != nil {
				return nil, err
			}
			var minf []byte
			for _, mk := range minfKids {
				if mk.typ == "stbl" {
					stbl, err := m.buildStbl(c, mk, lo, base)
					if err != nil {
						return nil, err
					}
					minf = append(minf, stbl...)
				} else {
					minf = append(minf, boxBytes(m.raw, mk)...)
				}
			}
			body = appendBox(body, "minf", minf)
		default:
			body = append(body, boxBytes(m.raw, k)...)
		}
	}
	return appendBox(nil, "mdia", body), nil
}

func (m *movie) buildStbl(c *trackCut, stbl box, lo, base int64) ([]byte, error) {
	t := c.t
	stsd, ok := findChild(m.raw, stbl.start, stbl.end, "stsd")
	if !ok {
		return nil, errors.New("missing stsd")
	}
	n := c.last - c.first + 1
	var body []byte
	body = append(body, boxBytes(m.raw, stsd)...)

	// stts: decode durations, run-length encoded.
	body = appendBox(body, "stts", rle(n, t.sttsCount, func(j int) uint32 { return t.sttsDelta[j] }, c.first, 0))

	// ctts: composition offsets.
	if t.cttsCount != nil {
		ver := t.cttsVersion
		for _, o := range t.cttsOffset {
			if o < 0 {
				ver = 1
			}
		}
		body = appendBox(body, "ctts", rle(n, t.cttsCount, func(j int) uint32 { return uint32(t.cttsOffset[j]) }, c.first, ver))
	}

	// stss: keyframes, renumbered.
	if t.sync != nil {
		var ks []uint32
		for _, s := range t.sync {
			if i := int(s) - 1; i >= c.first && i <= c.last {
				ks = append(ks, uint32(i-c.first+1))
			}
		}
		p := make([]byte, 0, 8+4*len(ks))
		p = binary.BigEndian.AppendUint32(p, 0)
		p = binary.BigEndian.AppendUint32(p, uint32(len(ks)))
		for _, k := range ks {
			p = binary.BigEndian.AppendUint32(p, k)
		}
		body = appendBox(body, "stss", p)
	}

	// stsz: sample sizes.
	p := make([]byte, 0, 12+4*n)
	p = binary.BigEndian.AppendUint32(p, 0)
	p = binary.BigEndian.AppendUint32(p, t.fixedSize)
	p = binary.BigEndian.AppendUint32(p, uint32(n))
	if t.fixedSize == 0 {
		for s := c.first; s <= c.last; s++ {
			p = binary.BigEndian.AppendUint32(p, t.sizes[s])
		}
	}
	body = appendBox(body, "stsz", p)

	// stsc: one sample per chunk, so every sample gets its own offset in co64.
	// This keeps the cut's edges exact without splitting the source's chunks.
	_, sdiEntry := chunkEntry(t, c.first)
	body = appendBox(body, "stsc", be32s(0, 1, 1, 1, t.stscSDI[sdiEntry]))

	// co64: where each sample lives in the new file.
	p = make([]byte, 0, 8+8*n)
	p = binary.BigEndian.AppendUint32(p, 0)
	p = binary.BigEndian.AppendUint32(p, uint32(n))
	for _, off := range c.offsets {
		p = binary.BigEndian.AppendUint64(p, uint64(base+off-lo))
	}
	body = appendBox(body, "co64", p)

	// Other sample tables (sdtp, sbgp, sgpd, ...) are indexed by sample number
	// and would be wrong for the cut, so they are dropped; players don't need them.
	return appendBox(nil, "stbl", body), nil
}

// chunkEntry returns the 0-based chunk holding sample i and the stsc entry for it.
func chunkEntry(t *track, i int) (chunk, entry int) {
	base := 0
	for j := range t.stscFirst {
		next := len(t.chunkOffsets) + 1
		if j+1 < len(t.stscFirst) {
			next = int(t.stscFirst[j+1])
		}
		per := int(t.stscPer[j])
		span := (next - int(t.stscFirst[j])) * per
		if i < base+span {
			return int(t.stscFirst[j]) - 1 + (i-base)/per, j
		}
		base += span
	}
	return len(t.chunkOffsets), len(t.stscFirst) - 1
}

// rle re-encodes n samples starting at sample `first` of a run-length table as a
// stts/ctts payload (version byte, flags, count, then count/value pairs).
func rle(n int, counts []uint32, value func(j int) uint32, first int, version byte) []byte {
	type run struct{ count, val uint32 }
	var runs []run
	j, within := runAt(counts, first)
	for left := n; left > 0 && j < len(counts); j, within = j+1, 0 {
		take := min(int(counts[j])-within, left)
		v := value(j)
		if len(runs) > 0 && runs[len(runs)-1].val == v {
			runs[len(runs)-1].count += uint32(take)
		} else {
			runs = append(runs, run{uint32(take), v})
		}
		left -= take
	}
	p := make([]byte, 0, 8+8*len(runs))
	p = append(p, version, 0, 0, 0)
	p = binary.BigEndian.AppendUint32(p, uint32(len(runs)))
	for _, r := range runs {
		p = binary.BigEndian.AppendUint32(p, r.count)
		p = binary.BigEndian.AppendUint32(p, r.val)
	}
	return p
}

func be32s(vs ...uint32) []byte {
	b := make([]byte, 0, 4*len(vs))
	for _, v := range vs {
		b = binary.BigEndian.AppendUint32(b, v)
	}
	return b
}
