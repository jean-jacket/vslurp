// vslurp downloads a time range of a large remote MP4 by reading its index and
// fetching only the bytes that range needs, in parallel and resumably, then cuts
// it locally with ffmpeg's stream copy.
//
// Usage: vslurp [flags] START-END INPUT OUTPUT
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const defaultUA = "vslurp/0.1 (ffmpeg-based MP4 cutter)"

type options struct {
	conns    int
	partMB   int
	ua       string
	cacheDir string
	pad      float64
	stream   bool
	output   string
}

func usage() {
	fmt.Fprintf(os.Stderr, `Usage: vslurp [flags] START-END INPUT OUTPUT
       vslurp watch [flags] URL     (play in mpv; see vslurp watch -h)

Downloads only the part of a remote MP4 needed for a time range, then cuts it
with ffmpeg's stream copy. Run the same command again to resume.

Time ranges:
  01:00:00-02:00:00   from 1h to 2h
  01:00:00-           from 1h to the end
  -01:00:00           from the start to 1h

INPUT is an http(s) URL. A local file is passed straight to ffmpeg.

With -stream, OUTPUT is written in playback order while downloading, so it can
be watched before the download finishes:  mpv appending://OUTPUT

Flags:
`)
	flag.PrintDefaults()
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "watch" {
		os.Exit(runWatch(os.Args[2:]))
	}
	var o options
	flag.IntVar(&o.conns, "j", 4, "number of parallel connections")
	flag.IntVar(&o.partMB, "part-size", 8, "download part size in MB")
	flag.StringVar(&o.ua, "user-agent", defaultUA, "User-Agent header to send")
	flag.StringVar(&o.cacheDir, "cache-dir", filepath.Join(os.TempDir(), "vslurp"), "where partial downloads are kept")
	flag.Float64Var(&o.pad, "pad", 2, "extra seconds fetched on each side of the range")
	flag.BoolVar(&o.stream, "stream", false, "write OUTPUT in playback order while downloading, so it can be\nwatched as it grows (mpv appending://OUTPUT); doesn't use ffmpeg")
	flag.Usage = usage

	flagArgs, pos, err := splitArgs(os.Args[1:])
	if err == nil {
		err = flag.CommandLine.Parse(flagArgs)
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
	} else {
		errorf("%v", err)
	}
	if err != nil || len(pos) != 3 {
		if err == nil {
			errorf("expected START-END INPUT OUTPUT, got %d argument(s)", len(pos))
		}
		usage()
		os.Exit(2)
	}
	if o.conns < 1 || o.partMB < 1 {
		errorf("-j and -part-size must be at least 1")
		os.Exit(2)
	}

	rng, err := parseRange(pos[0])
	if err != nil {
		errorf("%v", err)
		fmt.Fprintln(os.Stderr, "Valid ranges include: START-END, START- (until end), or -END (from start).")
		os.Exit(2)
	}
	input, output := pos[1], pos[2]
	o.output = output

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if !strings.HasPrefix(input, "http://") && !strings.HasPrefix(input, "https://") {
		if o.stream {
			errorf("-stream only works with http(s) URLs")
			os.Exit(2)
		}
		os.Exit(runFFmpeg(rng, input, output, false, ""))
	}
	if o.stream && !confirmOverwrite(output) {
		os.Exit(1)
	}

	requested := rng
	local, rng, err := slurp(ctx, o, input, rng)
	if err != nil {
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "\nInterrupted. Run the same command again to resume.")
			os.Exit(130)
		}
		errorf("%v", err)
		var fe fatalError
		if !errors.As(err, &fe) && !errors.Is(err, errChanged) {
			fmt.Fprintln(os.Stderr, "Run the same command again to resume.")
		}
		os.Exit(1)
	}
	if o.stream {
		fmt.Fprintf(os.Stderr, "Done: %s\n", output)
		return
	}
	stop() // let Ctrl-C reach ffmpeg normally
	os.Exit(runFFmpeg(rng, local, output, true, cutComment(input, requested, rng)))
}

// confirmOverwrite asks before replacing an existing regular file, like ffmpeg.
func confirmOverwrite(path string) bool {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return true // doesn't exist, or a named pipe/device
	}
	in, err := os.Stdin.Stat()
	if err != nil || in.Mode()&os.ModeCharDevice == 0 {
		errorf("%s already exists", path)
		return false
	}
	fmt.Fprintf(os.Stderr, "File '%s' already exists. Overwrite? [y/N] ", path)
	var ans string
	fmt.Scanln(&ans)
	if a := strings.ToLower(strings.TrimSpace(ans)); a == "y" || a == "yes" {
		return true
	}
	fmt.Fprintln(os.Stderr, "Not overwriting.")
	return false
}

func errorf(format string, a ...any) {
	if stderrIsTTY {
		fmt.Fprintf(os.Stderr, "\033[31mError: "+format+"\033[39m\n", a...)
	} else {
		fmt.Fprintf(os.Stderr, "Error: "+format+"\n", a...)
	}
}

// splitArgs separates flags from positional arguments. Positional arguments may
// appear anywhere, and a range like "-01:00:00" is positional, not a flag.
func splitArgs(args []string) (flags, pos []string, err error) {
	takesValue := map[string]bool{"j": true, "part-size": true, "user-agent": true, "cache-dir": true, "pad": true, "stream": false}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if len(a) < 2 || a[0] != '-' || isRangeArg(a) {
			pos = append(pos, a)
			continue
		}
		flags = append(flags, a)
		name, _, hasEq := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if name == "h" || name == "help" {
			continue
		}
		needsValue, ok := takesValue[name]
		if !ok {
			return nil, nil, fmt.Errorf("unknown flag %s", a)
		}
		if needsValue && !hasEq {
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("flag %s needs a value", a)
			}
			i++
			flags = append(flags, args[i])
		}
	}
	return flags, pos, nil
}

func isRangeArg(a string) bool {
	return len(a) > 1 && a[0] == '-' && (a[1] >= '0' && a[1] <= '9' || a[1] == '.')
}

// timeRange keeps the user's original strings (passed to ffmpeg unchanged)
// alongside their values in seconds. A missing side is -1.
type timeRange struct {
	startStr, endStr string
	start, end       float64
}

func parseRange(s string) (timeRange, error) {
	startStr, endStr, _ := strings.Cut(s, "-")
	r := timeRange{startStr: startStr, endStr: endStr, start: -1, end: -1}
	if startStr == "" && endStr == "" {
		return r, fmt.Errorf("the time range is invalid (%q). At least START or END time is required", s)
	}
	var err error
	if startStr != "" {
		if r.start, err = parseTime(startStr); err != nil {
			return r, err
		}
	}
	if endStr != "" {
		if r.end, err = parseTime(endStr); err != nil {
			return r, err
		}
	}
	if r.start >= 0 && r.end >= 0 && r.end <= r.start {
		return r, fmt.Errorf("end time %s is not after start time %s", endStr, startStr)
	}
	return r, nil
}

// parseTime accepts ffmpeg-style [[HH:]MM:]SS[.frac].
func parseTime(s string) (float64, error) {
	parts := strings.Split(s, ":")
	if len(parts) > 3 {
		return 0, fmt.Errorf("invalid time %q", s)
	}
	total := 0.0
	for i, p := range parts {
		v, err := strconv.ParseFloat(p, 64)
		if err != nil || v < 0 || p == "" || (i < len(parts)-1 && strings.Contains(p, ".")) {
			return 0, fmt.Errorf("invalid time %q (use HH:MM:SS, MM:SS or seconds)", s)
		}
		total = total*60 + v
	}
	return total, nil
}

// runFFmpeg cuts with stream copy. snapped means the start is exactly on a
// keyframe, so timestamps can safely be shifted to begin at zero.
func runFFmpeg(r timeRange, input, output string, snapped bool, comment string) int {
	args := []string{"-hide_banner", "-loglevel", "warning"}
	if r.startStr != "" {
		args = append(args, "-ss", r.startStr)
	}
	if r.endStr != "" {
		args = append(args, "-to", r.endStr)
	}
	args = append(args, "-i", input, "-c", "copy")
	if snapped {
		args = append(args, "-avoid_negative_ts", "make_zero")
	}
	if comment != "" {
		args = append(args, "-metadata", "comment="+comment)
	}
	args = append(args, output)
	fmt.Fprintf(os.Stderr, "Cutting %s with ffmpeg...\n", output)
	cmd := exec.Command("ffmpeg", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		errorf("running ffmpeg: %v", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "Done: %s\n", output)
	return 0
}

// meta is saved next to the cached data so a later run can resume.
type meta struct {
	URL       string `json:"url"`
	Length    int64  `json:"length"`
	ETag      string `json:"etag"`
	MdatStart int64  `json:"mdat_start"`
	MdatEnd   int64  `json:"mdat_end"`
	MoovStart int64  `json:"moov_start"`
	MoovEnd   int64  `json:"moov_end"`
}

// slurp makes sure the local sparse copy holds everything needed for the cut. It
// returns its path and the range to cut, with the start moved onto a keyframe.
func slurp(ctx context.Context, o options, url string, rng timeRange) (path string, snapped timeRange, err error) {
	path, err = slurpInner(ctx, o, url, &rng)
	return path, rng, err
}

func slurpInner(ctx context.Context, o options, url string, rng *timeRange) (string, error) {
	requested := *rng
	src, err := openSource(ctx, o, url)
	if err != nil {
		return "", err
	}
	defer src.st.close()
	r, m, st, d, dataPath, mov := src.r, src.m, src.st, src.d, src.dataPath, src.mov
	dur := mov.duration()
	if rng.start >= dur {
		return "", fatalError{fmt.Errorf("start time %s is past the end of the video (%s)", rng.startStr, fmtSeconds(dur))}
	}

	// A stream copy can only start on a keyframe. Starting anywhere else makes
	// ffmpeg copy from the previous keyframe and hide the extra frames with
	// negative timestamps and an edit list, which many players handle badly.
	// So move the start onto the keyframe ourselves.
	if rng.start > 0 {
		if s, ok := mov.snapToKeyframe(rng.start); ok {
			str := fmtFFTime(s)
			if diff := rng.start - s; diff > 0.0005 {
				fmt.Fprintf(os.Stderr, "Start moved to keyframe at %s (%.3f s before %s).\n", str, diff, rng.startStr)
			}
			rng.start, rng.startStr = s, str
		}
	}

	if o.stream {
		return dataPath, streamCut(ctx, d, st, mov, *rng, o.output, dur, r.length, cutComment(url, requested, *rng))
	}

	// 2. The bytes for the requested time range.
	lo, hi, err := mov.byteRange(rng.start, rng.end, o.pad)
	if err != nil {
		return "", fatalError{err}
	}
	lo = max(lo-(256<<10), m.MdatStart)
	hi = min(hi+(256<<10), m.MdatEnd)
	fmt.Fprintf(os.Stderr, "Video is %s long; the cut needs %s of %s.\n", fmtSeconds(dur), fmtBytes(hi-lo), fmtBytes(r.length))
	if err := d.fetch(ctx, "video", []span{{lo, hi}}); err != nil {
		return "", err
	}
	return dataPath, nil
}

// source is a remote MP4 with its local cache opened and its index loaded.
type source struct {
	r        *remote
	m        meta
	st       *store
	d        *downloader
	dataPath string
	mov      *movie
	lock     *os.File
}

// unlock lets other vslurp processes download this file.
func (s *source) unlock() {
	syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
	s.lock.Close()
}

// openSource probes the URL, opens (or resets) its cache folder, makes sure the
// file's header and index are downloaded, and parses the index. It returns with
// the cache folder locked.
func openSource(ctx context.Context, o options, url string) (*source, error) {
	r := newRemote(url, o.ua, o.conns)
	head, err := r.probe(ctx)
	if err != nil {
		return nil, err
	}
	if len(head) < 8 || string(head[4:8]) != "ftyp" {
		return nil, fatalError{errors.New("this doesn't look like an MP4 file (no ftyp box at the start)")}
	}

	sum := sha256.Sum256([]byte(url))
	dir := filepath.Join(o.cacheDir, hex.EncodeToString(sum[:8]))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, fatalError{errors.New("another vslurp is already downloading this file")}
	}
	// For a cut the lock is held until the process exits; watch releases it
	// once the index is in place, since it never writes video data.

	metaPath := filepath.Join(dir, "meta.json")
	var m meta
	if b, err := os.ReadFile(metaPath); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	if m.URL != url || m.Length != r.length || m.ETag != r.etag || m.MoovEnd == 0 {
		// New file, or the remote changed: start over.
		for _, f := range []string{"data.mp4", "have.log", "meta.json"} {
			os.Remove(filepath.Join(dir, f))
		}
		m, err = layout(ctx, r)
		if err != nil {
			return nil, err
		}
		m.URL = url
		b, _ := json.MarshalIndent(m, "", "  ")
		if err := os.WriteFile(metaPath, b, 0o644); err != nil {
			return nil, err
		}
	}

	dataPath := filepath.Join(dir, "data.mp4")
	st, err := openStore(dataPath, filepath.Join(dir, "have.log"), r.length)
	if err != nil {
		return nil, err
	}
	d := &downloader{r: r, st: st, conns: o.conns, partSize: int64(o.partMB) << 20}

	// 1. The header and the index (moov). A little of the start of mdat is
	//    included too, since ffmpeg may peek at it when opening the file.
	headEnd := min(m.MdatStart+(1<<20), m.MdatEnd)
	if err := d.fetch(ctx, "index", []span{{0, headEnd}, {m.MoovStart, m.MoovEnd}}); err != nil {
		st.close()
		return nil, err
	}
	moov := make([]byte, m.MoovEnd-m.MoovStart)
	if _, err := st.data.ReadAt(moov, m.MoovStart); err != nil {
		st.close()
		return nil, err
	}
	mov, err := parseMoov(moov)
	if err != nil {
		st.close()
		return nil, fatalError{err}
	}
	return &source{r: r, m: m, st: st, d: d, dataPath: dataPath, mov: mov, lock: lock}, nil
}

// layout walks the top-level boxes with tiny range requests to find mdat and moov.
func layout(ctx context.Context, r *remote) (meta, error) {
	m := meta{Length: r.length, ETag: r.etag}
	for off, n := int64(0), 0; off < r.length; n++ {
		if n > 10000 {
			return m, fatalError{errors.New("too many top-level boxes")}
		}
		hdr, err := r.readAt(ctx, off, min(off+16, r.length))
		if err != nil {
			return m, err
		}
		bx, err := parseBoxHeader(hdr, off, r.length)
		if err != nil {
			return m, fatalError{err}
		}
		switch bx.typ {
		case "moov":
			m.MoovStart, m.MoovEnd = bx.offset, bx.end
		case "mdat":
			if m.MdatEnd == 0 {
				m.MdatStart, m.MdatEnd = bx.start, bx.end
			}
		case "moof":
			return m, fatalError{errors.New("fragmented MP4 (moof boxes) is not supported")}
		}
		off = bx.end
	}
	if m.MoovEnd == 0 || m.MdatEnd == 0 {
		return m, fatalError{errors.New("couldn't find both the moov and mdat boxes")}
	}
	return m, nil
}

// fmtFFTime formats seconds as HH:MM:SS.ffffff for ffmpeg, rounding up to the
// microsecond so the time is never just before the keyframe it names (which
// would make ffmpeg seek back to the previous one).
func fmtFFTime(s float64) string {
	us := int64(math.Ceil(s*1e6 - 1e-3))
	sec := us / 1e6
	return fmt.Sprintf("%02d:%02d:%02d.%06d", sec/3600, sec/60%60, sec%60, us%1e6)
}

func fmtSeconds(s float64) string {
	t := int(s)
	return fmt.Sprintf("%d:%02d:%02d", t/3600, t/60%60, t%60)
}

// streamCut writes the cut as a fast-start MP4: the new index first, then the
// media bytes in order, each stretch as soon as it has been downloaded.
func streamCut(ctx context.Context, d *downloader, st *store, mov *movie, rng timeRange, output string, dur float64, total int64, comment string) error {
	ftypHdr := make([]byte, 16)
	if _, err := st.data.ReadAt(ftypHdr, 0); err != nil {
		return err
	}
	fb, err := parseBoxHeader(ftypHdr, 0, total)
	if err != nil || fb.typ != "ftyp" || fb.end > 4096 {
		return fatalError{errors.New("unexpected ftyp box")}
	}
	ftyp := make([]byte, fb.end)
	if _, err := st.data.ReadAt(ftyp, 0); err != nil {
		return err
	}

	start, end := max(rng.start, 0), rng.end
	if end >= dur {
		end = -1
	}
	plan, err := mov.planCut(ftyp, start, end, comment)
	if err != nil {
		return fatalError{err}
	}
	fmt.Fprintf(os.Stderr, "Video is %s long; the cut needs %s of %s.\n", fmtSeconds(dur), fmtBytes(plan.hi-plan.lo), fmtBytes(total))

	out, err := os.Create(output) // also works for a named pipe (blocks until a reader opens it)
	if err != nil {
		return fatalError{err}
	}
	defer out.Close()
	if _, err := out.Write(plan.header); err != nil {
		return fatalError{err}
	}
	fmt.Fprintf(os.Stderr, "Writing %s as it downloads. Watch it with:  mpv appending://%s\n", output, output)

	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	written := make(chan error, 1)
	want := span{plan.lo, plan.hi}
	go func() { written <- st.copyOut(wctx, out, want) }()

	if err := d.fetch(ctx, "video", []span{want}); err != nil {
		cancel()
		<-written
		return err
	}
	if err := <-written; err != nil {
		return fatalError{err}
	}
	return out.Close()
}

// cutComment describes where a cut came from, for the output's comment tag:
// the source URL and the range actually cut (plus the requested one if the
// start was moved onto a keyframe).
func cutComment(url string, requested, actual timeRange) string {
	side := func(s, missing string) string {
		if s == "" {
			return missing
		}
		if strings.Contains(s, ".") { // drop trailing zeros from snapped times
			s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
		}
		return s
	}
	got := side(actual.startStr, "start") + "-" + side(actual.endStr, "end")
	asked := side(requested.startStr, "start") + "-" + side(requested.endStr, "end")
	line := "Range: " + got
	if got != asked {
		line += " (requested " + asked + ")"
	}
	return "Source: " + url + "\n" + line
}
