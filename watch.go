package main

// vslurp watch: serve a remote MP4 to mpv from a local web server that only this
// machine can reach. The index comes from the on-disk cache; video data is
// fetched on demand into a fixed-size memory buffer with read-ahead, and is never
// written to disk.

import (
	"container/list"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

func watchUsage(fs *flag.FlagSet) func() {
	return func() {
		fmt.Fprintf(os.Stderr, `Usage: vslurp watch [flags] URL [-- MPV_ARGS...]

Plays a remote MP4 in mpv through a local server (127.0.0.1 only) that fetches
data efficiently: the index is cached on disk, video data is downloaded on
demand into memory with read-ahead and is never saved. If mpv can't be started,
the local URL is printed instead. Arguments in -mpv-args, then any after --,
are passed to mpv.

Flags:
`)
		fs.PrintDefaults()
	}
}

type watchOpts struct {
	options
	noMPV       bool
	memMB       int
	readaheadMB int
	partMB      int
	mpvArgs     []string
}

func runWatch(args []string) int {
	var o watchOpts
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.IntVar(&o.conns, "j", 4, "number of parallel connections")
	fs.StringVar(&o.ua, "user-agent", defaultUA, "User-Agent header to send")
	fs.StringVar(&o.cacheDir, "cache-dir", filepath.Join(os.TempDir(), "vslurp"), "where the index is cached")
	fs.BoolVar(&o.noMPV, "no-mpv", false, "don't start mpv; just print the local URL")
	fs.IntVar(&o.memMB, "mem", 256, "memory buffer for video data, in MB")
	fs.IntVar(&o.readaheadMB, "readahead", 64, "how far ahead of the player to download, in MB")
	fs.IntVar(&o.partMB, "part-size", 2, "download part size in MB")
	var mpvFlag string
	fs.StringVar(&mpvFlag, "mpv-args", "", "extra arguments for mpv, as one string (e.g. \"--hr-seek=no --volume=50\")")
	fs.Usage = watchUsage(fs)

	if i := indexOf(args, "--"); i >= 0 {
		o.mpvArgs = args[i+1:]
		args = args[:i]
	}
	// Allow flags before and after the URL.
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			return 2
		}
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
	if mpvFlag != "" {
		words, err := splitWords(mpvFlag)
		if err != nil {
			errorf("-mpv-args: %v", err)
			return 2
		}
		o.mpvArgs = append(words, o.mpvArgs...)
	}
	if len(pos) != 1 {
		errorf("expected exactly one URL, got %d argument(s)", len(pos))
		fs.Usage()
		return 2
	}
	url := pos[0]
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		errorf("watch needs an http(s) URL; play local files with mpv directly")
		return 2
	}
	if o.conns < 1 || o.memMB < 1 || o.partMB < 1 || o.readaheadMB < 0 {
		errorf("-j, -mem and -part-size must be at least 1")
		return 2
	}
	if o.memMB < 2*o.partMB+o.readaheadMB {
		o.readaheadMB = max(o.memMB-2*o.partMB, 0)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	o.options.partMB = 8 // the index download uses the normal part size
	src, err := openSource(ctx, o.options, url)
	if err != nil {
		if ctx.Err() != nil {
			return 130
		}
		errorf("%v", err)
		return 1
	}
	defer src.st.close()
	src.unlock() // watch never writes video data, so cuts can run alongside it

	w := newWatcher(src, int64(o.partMB)<<20, o.memMB, o.readaheadMB, o.conns)
	wctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.start(wctx)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		errorf("starting local server: %v", err)
		return 1
	}
	token := make([]byte, 16)
	rand.Read(token)
	name := fileName(url)
	urlPath := "/" + hex.EncodeToString(token) + "/" + neturl.PathEscape(name)
	local := "http://" + ln.Addr().String() + urlPath

	srv := &http.Server{
		Handler:           w.handler(urlPath, name),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go srv.Serve(ln)
	defer srv.Close()

	fmt.Fprintf(os.Stderr, "Video is %s long. Serving it at:\n  %s\n", fmtSeconds(src.mov.duration()), local)

	if !o.noMPV {
		code, started := runMPV(o.mpvArgs, name, local)
		if started && code != 1 && code != 2 {
			return 0 // mpv quit normally (or was interrupted)
		}
		if started {
			fmt.Fprintln(os.Stderr, "mpv couldn't play it.")
		} else {
			fmt.Fprintln(os.Stderr, "Couldn't start mpv.")
		}
		fmt.Fprintf(os.Stderr, "Open this URL in a player instead:\n  %s\n", local)
	}
	fmt.Fprintln(os.Stderr, "Press Ctrl-C to stop.")
	w.statusUntil(ctx)
	return 0
}

// splitWords splits s into arguments like a shell would: on whitespace, with
// single quotes, double quotes and backslash escapes. No variables or globs.
func splitWords(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	var quote rune
	escaped := false
	for _, c := range s {
		switch {
		case escaped:
			cur.WriteRune(c)
			escaped = false
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				cur.WriteRune(c)
			}
		case c == '\\':
			escaped, inWord = true, true
		case quote == '"':
			if c == '"' {
				quote = 0
			} else {
				cur.WriteRune(c)
			}
		case c == '\'' || c == '"':
			quote, inWord = c, true
		case c == ' ' || c == '\t' || c == '\n':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(c)
			inWord = true
		}
	}
	if quote != 0 || escaped {
		return nil, errors.New("unterminated quote or trailing backslash")
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}

func indexOf(xs []string, s string) int {
	for i, x := range xs {
		if x == s {
			return i
		}
	}
	return -1
}

// fileName picks a readable name for the served file from the remote URL.
func fileName(raw string) string {
	name := "video.mp4"
	if u, err := neturl.Parse(raw); err == nil {
		if b := path.Base(u.Path); b != "" && b != "/" && b != "." {
			name = b
		}
	}
	if !strings.HasSuffix(strings.ToLower(name), ".mp4") {
		name += ".mp4"
	}
	return name
}

// runMPV starts mpv on the local URL and waits for it. started is false if mpv
// couldn't be launched at all.
func runMPV(extra []string, title, local string) (code int, started bool) {
	bin, err := exec.LookPath("mpv")
	if err != nil {
		return 0, false
	}
	// Our title goes first so the user's own arguments can override it.
	args := append(append([]string{"--force-media-title=" + title}, extra...), local)
	cmd := exec.Command(bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// Ctrl-C in the terminal reaches mpv too; let mpv decide when we're done.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	if err := cmd.Start(); err != nil {
		return 0, false
	}
	err = cmd.Wait()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), true
	}
	return 0, true
}

// ---------- memory buffer ----------

type memPart struct {
	idx  int64
	done chan struct{}
	data []byte
	err  error
	elem *list.Element
}

type watcher struct {
	src       *source
	length    int64
	partSize  int64
	nParts    int64
	maxParts  int
	readahead int64 // in parts
	conns     int

	mu     sync.Mutex
	parts  map[int64]*memPart
	lru    *list.List // front = most recently used
	cursor int64      // part the player read most recently (-1: none yet)
	wake   chan struct{}

	downloaded atomic.Int64
	ctx        context.Context
}

func newWatcher(src *source, partSize int64, memMB, readaheadMB, conns int) *watcher {
	length := src.r.length
	return &watcher{
		src:       src,
		length:    length,
		partSize:  partSize,
		nParts:    (length + partSize - 1) / partSize,
		maxParts:  max(int((int64(memMB)<<20)/partSize), 2),
		readahead: (int64(readaheadMB) << 20) / partSize,
		conns:     conns,
		parts:     map[int64]*memPart{},
		lru:       list.New(),
		cursor:    -1,
		wake:      make(chan struct{}, 1),
	}
}

func (w *watcher) start(ctx context.Context) {
	w.ctx = ctx
	for range w.conns {
		go w.prefetchLoop()
	}
}

func (w *watcher) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// onDisk reports whether part idx is entirely in the on-disk cache already
// (from the index or earlier cuts).
func (w *watcher) onDisk(idx int64) bool {
	a := idx * w.partSize
	b := min(a+w.partSize, w.length)
	return w.src.st.coveredUntil(a) >= b
}

// claim returns the part for idx, creating it (and reporting created=true) if
// it isn't in memory or being downloaded. Must hold w.mu.
func (w *watcher) claim(idx int64) (p *memPart, created bool) {
	if p := w.parts[idx]; p != nil {
		w.lru.MoveToFront(p.elem)
		return p, false
	}
	p = &memPart{idx: idx, done: make(chan struct{})}
	p.elem = w.lru.PushFront(p)
	w.parts[idx] = p
	w.evict()
	return p, true
}

// evict drops the least recently used finished parts beyond the memory limit.
func (w *watcher) evict() {
	for e := w.lru.Back(); e != nil && w.lru.Len() > w.maxParts; {
		prev := e.Prev()
		p := e.Value.(*memPart)
		select {
		case <-p.done: // finished: safe to drop (readers keep their own reference)
			w.lru.Remove(e)
			delete(w.parts, p.idx)
		default: // still downloading
		}
		e = prev
	}
}

func (w *watcher) fetch(p *memPart) {
	a := p.idx * w.partSize
	b := min(a+w.partSize, w.length)
	ctx, cancel := context.WithTimeout(w.ctx, 90*time.Second)
	defer cancel()
	p.data, p.err = w.src.r.readAt(ctx, a, b)
	if p.err == nil {
		w.downloaded.Add(b - a)
	} else {
		// Forget the failed part so a later read retries it.
		w.mu.Lock()
		if w.parts[p.idx] == p {
			w.lru.Remove(p.elem)
			delete(w.parts, p.idx)
		}
		w.mu.Unlock()
	}
	close(p.done)
	w.signal()
}

// prefetchLoop keeps the parts just ahead of the player downloaded.
func (w *watcher) prefetchLoop() {
	for {
		w.mu.Lock()
		var next *memPart
		if w.cursor >= 0 {
			for i := w.cursor; i <= w.cursor+w.readahead && i < w.nParts; i++ {
				if w.parts[i] == nil && !w.onDisk(i) {
					next, _ = w.claim(i)
					break
				}
			}
		}
		w.mu.Unlock()
		if next != nil {
			w.fetch(next)
			continue
		}
		select {
		case <-w.wake:
		case <-w.ctx.Done():
			return
		}
	}
}

// readAt serves a read from disk or memory, downloading missing parts first.
func (w *watcher) readAt(ctx context.Context, buf []byte, off int64) (int, error) {
	if off >= w.length {
		return 0, io.EOF
	}
	want := buf[:min(int64(len(buf)), w.length-off)]
	n := 0
	for n < len(want) {
		pos := off + int64(n)
		if end := w.src.st.coveredUntil(pos); end > pos {
			k := min(end-pos, int64(len(want)-n))
			m, err := w.src.st.data.ReadAt(want[n:n+int(k)], pos)
			n += m
			if err != nil {
				return n, err
			}
			continue
		}
		idx := pos / w.partSize
		w.mu.Lock()
		p, created := w.claim(idx)
		moved := w.cursor != idx
		w.cursor = idx
		w.mu.Unlock()
		if moved {
			w.signal() // read-ahead follows the player
		}
		if created {
			go w.fetch(p) // the player is waiting for this one: don't queue it
		}
		select {
		case <-p.done:
		case <-ctx.Done():
			return n, ctx.Err()
		}
		if p.err != nil {
			return n, p.err
		}
		n += copy(want[n:], p.data[pos-idx*w.partSize:])
	}
	if n < len(buf) {
		return n, io.EOF
	}
	return n, nil
}

// reqReader ties reads to one HTTP request, so they stop when the player
// disconnects (for example after a seek).
type reqReader struct {
	w   *watcher
	ctx context.Context
}

func (r reqReader) ReadAt(p []byte, off int64) (int, error) { return r.w.readAt(r.ctx, p, off) }

func (w *watcher) handler(urlPath, name string) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if req.URL.EscapedPath() != urlPath && req.URL.Path != urlPath {
			http.NotFound(rw, req)
			return
		}
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		rw.Header().Set("Content-Type", "video/mp4")
		body := io.NewSectionReader(reqReader{w, req.Context()}, 0, w.length)
		http.ServeContent(rw, req, name, time.Time{}, body)
	})
}

// statusUntil shows download activity until ctx is cancelled (used when mpv
// isn't attached to the terminal).
func (w *watcher) statusUntil(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	last, lastT := w.downloaded.Load(), time.Now()
	for {
		select {
		case <-ctx.Done():
			if stderrIsTTY {
				fmt.Fprintln(os.Stderr)
			}
			return
		case now := <-tick.C:
			d := w.downloaded.Load()
			speed := float64(d-last) / now.Sub(lastT).Seconds()
			last, lastT = d, now
			w.mu.Lock()
			inMem := int64(w.lru.Len()) * w.partSize
			w.mu.Unlock()
			line := fmt.Sprintf("watch: %s/s, %s downloaded, %s in memory",
				fmtBytes(int64(speed)), fmtBytes(d), fmtBytes(inMem))
			if stderrIsTTY {
				fmt.Fprintf(os.Stderr, "\r\033[K%s", line)
			} else if speed > 0 {
				fmt.Fprintln(os.Stderr, line)
			}
		}
	}
}
