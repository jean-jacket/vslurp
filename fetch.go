package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- intervals ----------

// span is a half-open byte range [a, b).
type span struct{ a, b int64 }

// spans is a sorted list of non-overlapping, non-adjacent spans.
type spans []span

func (s spans) add(n span) spans {
	if n.b <= n.a {
		return s
	}
	out := make(spans, 0, len(s)+1)
	for _, x := range s {
		if x.b < n.a || x.a > n.b { // disjoint and not touching
			out = append(out, x)
			continue
		}
		n.a, n.b = min(n.a, x.a), max(n.b, x.b)
	}
	out = append(out, n)
	sort.Slice(out, func(i, j int) bool { return out[i].a < out[j].a })
	return out
}

// missing returns the parts of want not covered by s.
func (s spans) missing(want span) spans {
	var out spans
	cur := want.a
	for _, x := range s {
		if x.b <= cur || x.a >= want.b {
			continue
		}
		if x.a > cur {
			out = append(out, span{cur, x.a})
		}
		cur = max(cur, x.b)
	}
	if cur < want.b {
		out = append(out, span{cur, want.b})
	}
	return out
}

func (s spans) total() (n int64) {
	for _, x := range s {
		n += x.b - x.a
	}
	return
}

// ---------- remote ----------

type remote struct {
	url    string
	ua     string
	etag   string
	length int64
	client *http.Client
}

var errChanged = errors.New("the remote file changed while downloading; run the same command again to start fresh")

type fatalError struct{ error }

func newRemote(url, ua string, conns int) *remote {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	// HTTP/2 would multiplex every request over one TCP connection, which defeats
	// the point of parallel downloads. Force HTTP/1.1 so each worker gets its own.
	// The clone inherits TLS settings that still advertise h2, so offer only
	// http/1.1 during the TLS handshake.
	tr.TLSClientConfig = &tls.Config{NextProtos: []string{"http/1.1"}}
	tr.Protocols = new(http.Protocols)
	tr.Protocols.SetHTTP1(true)
	tr.MaxIdleConnsPerHost = conns + 2
	tr.ResponseHeaderTimeout = 30 * time.Second
	return &remote{url: url, ua: ua, client: &http.Client{Transport: tr}}
}

// get issues a range request for [a, b) and returns the response body.
// The caller must close it.
func (r *remote) get(ctx context.Context, a, b int64) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return nil, fatalError{err}
	}
	req.Header.Set("User-Agent", r.ua)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", a, b-1))
	if r.etag != "" {
		req.Header.Set("If-Range", r.etag)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusPartialContent:
	case resp.StatusCode == http.StatusOK && r.etag != "":
		resp.Body.Close()
		return nil, fatalError{errChanged}
	case resp.StatusCode == http.StatusOK:
		resp.Body.Close()
		return nil, fatalError{errors.New("server ignored the Range header; it does not support partial downloads")}
	case resp.StatusCode == 408 || resp.StatusCode == 429 || resp.StatusCode >= 500:
		resp.Body.Close()
		return nil, &retryAfter{status: resp.Status, after: parseRetryAfter(resp.Header.Get("Retry-After"))}
	default:
		resp.Body.Close()
		return nil, fatalError{fmt.Errorf("HTTP %s", resp.Status)}
	}
	start, _, total, ok := parseContentRange(resp.Header.Get("Content-Range"))
	if !ok || start != a {
		resp.Body.Close()
		return nil, fatalError{fmt.Errorf("unexpected Content-Range %q", resp.Header.Get("Content-Range"))}
	}
	if r.length != 0 && total != r.length {
		resp.Body.Close()
		return nil, fatalError{errChanged}
	}
	return resp.Body, nil
}

type retryAfter struct {
	status string
	after  time.Duration
}

func (e *retryAfter) Error() string { return "HTTP " + e.status }

func parseRetryAfter(v string) time.Duration {
	if s, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && s >= 0 {
		return time.Duration(s) * time.Second
	}
	return 0
}

func parseContentRange(v string) (start, end, total int64, ok bool) {
	// "bytes 0-15/66187559384"
	v, found := strings.CutPrefix(v, "bytes ")
	if !found {
		return
	}
	rng, tot, found := strings.Cut(v, "/")
	if !found {
		return
	}
	s, e, found := strings.Cut(rng, "-")
	if !found {
		return
	}
	var err1, err2, err3 error
	start, err1 = strconv.ParseInt(s, 10, 64)
	end, err2 = strconv.ParseInt(e, 10, 64)
	total, err3 = strconv.ParseInt(tot, 10, 64)
	return start, end, total, err1 == nil && err2 == nil && err3 == nil
}

// probe fetches the first bytes and learns the file's length and ETag.
func (r *remote) probe(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", r.ua)
	req.Header.Set("Range", "bytes=0-15")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("range request returned HTTP %s (the server must support byte ranges)", resp.Status)
	}
	_, _, total, ok := parseContentRange(resp.Header.Get("Content-Range"))
	if !ok {
		return nil, fmt.Errorf("unexpected Content-Range %q", resp.Header.Get("Content-Range"))
	}
	r.length = total
	r.etag = resp.Header.Get("ETag")
	if strings.HasPrefix(r.etag, "W/") {
		r.etag = "" // weak ETags can't be used with If-Range
	}
	return io.ReadAll(io.LimitReader(resp.Body, 16))
}

// readAt reads a small range into memory, with retries.
func (r *remote) readAt(ctx context.Context, a, b int64) ([]byte, error) {
	var last error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, backoff(attempt, 0)); err != nil {
				return nil, err
			}
		}
		body, err := r.get(ctx, a, b)
		if err == nil {
			buf, err2 := io.ReadAll(body)
			body.Close()
			if err2 == nil && int64(len(buf)) == b-a {
				return buf, nil
			}
			err = err2
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
		}
		var fe fatalError
		if errors.As(err, &fe) || ctx.Err() != nil {
			return nil, err
		}
		last = err
	}
	return nil, last
}

// ---------- cache ----------

// store is the local sparse copy of the remote file plus a log of which
// byte ranges have been downloaded and synced.
type store struct {
	data *os.File
	log  *os.File
	mu   sync.Mutex
	have spans
	// changed gets a (non-blocking) signal whenever a part is marked done.
	changed chan struct{}
}

func openStore(dataPath, logPath string, length int64) (*store, error) {
	data, err := os.OpenFile(dataPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if st, _ := data.Stat(); st.Size() != length {
		if err := data.Truncate(length); err != nil { // sparse: holes take no disk space
			data.Close()
			return nil, err
		}
	}
	s := &store{data: data, changed: make(chan struct{}, 1)}
	if f, err := os.Open(logPath); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var a, b int64
			// A torn last line from a crash simply fails to parse and is ignored.
			if n, _ := fmt.Sscanf(sc.Text(), "%d %d", &a, &b); n == 2 && a >= 0 && b <= length {
				s.have = s.have.add(span{a, b})
			}
		}
		f.Close()
	}
	s.log, err = os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		data.Close()
		return nil, err
	}
	return s, nil
}

func (s *store) markDone(sp span) error {
	// Make sure the bytes are on disk before recording them as present.
	if err := s.data.Sync(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.have = s.have.add(sp)
	_, err := fmt.Fprintf(s.log, "%d %d\n", sp.a, sp.b)
	select {
	case s.changed <- struct{}{}:
	default:
	}
	return err
}

// coveredUntil returns the end of the downloaded run starting at x (x itself if
// byte x isn't downloaded yet).
func (s *store) coveredUntil(x int64) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sp := range s.have {
		if sp.a <= x && x < sp.b {
			return sp.b
		}
	}
	return x
}

// copyOut appends the bytes of want to out in order, each stretch as soon as
// it and everything before it has been downloaded.
func (s *store) copyOut(ctx context.Context, out io.Writer, want span) error {
	for pos := want.a; pos < want.b; {
		if end := min(s.coveredUntil(pos), want.b); end > pos {
			if _, err := io.Copy(out, io.NewSectionReader(s.data, pos, end-pos)); err != nil {
				return err
			}
			pos = end
			continue
		}
		select {
		case <-s.changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (s *store) missing(want span) spans {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.have.missing(want)
}

func (s *store) close() {
	s.data.Close()
	s.log.Close()
}

// ---------- downloader ----------

type downloader struct {
	r        *remote
	st       *store
	conns    int
	partSize int64
}

// fetch makes sure every byte in wants is present locally.
func (d *downloader) fetch(ctx context.Context, label string, wants []span) error {
	var todo spans
	var wantTotal int64
	for _, w := range wants {
		wantTotal += w.b - w.a
		for _, m := range d.st.missing(w) {
			for a := m.a; a < m.b; a += d.partSize {
				todo = append(todo, span{a, min(a+d.partSize, m.b)})
			}
		}
	}
	missingTotal := todo.total()
	p := newProgress(label, wantTotal, wantTotal-missingTotal)
	if len(todo) == 0 {
		p.finish()
		return nil
	}
	p.start()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan span)
	var firstErr error
	var errOnce sync.Once
	fail := func(err error) {
		errOnce.Do(func() { firstErr = err; cancel() })
	}
	var wg sync.WaitGroup
	for range min(d.conns, len(todo)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for part := range jobs {
				if err := d.fetchPart(ctx, part, p); err != nil {
					fail(err)
					return
				}
			}
		}()
	}
feed:
	for _, part := range todo {
		select {
		case jobs <- part:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		p.stop()
		return firstErr
	}
	p.finish()
	return nil
}

func (d *downloader) fetchPart(ctx context.Context, part span, p *progress) error {
	got := int64(0)
	failures := 0
	for got < part.b-part.a {
		n, err := d.stream(ctx, part.a+got, part.b, p)
		got += n
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var fe fatalError
		if errors.As(err, &fe) {
			return fe.error
		}
		if n > 0 {
			failures = 0 // made progress; only count consecutive failures
		}
		failures++
		if failures > 8 {
			return fmt.Errorf("giving up on bytes %d-%d after repeated errors: %w", part.a, part.b-1, err)
		}
		var ra *retryAfter
		wait := time.Duration(0)
		if errors.As(err, &ra) {
			wait = ra.after
		}
		p.note(fmt.Sprintf("retrying after error: %v", err))
		if err := sleepCtx(ctx, backoff(failures, wait)); err != nil {
			return err
		}
	}
	return d.st.markDone(part)
}

// stream downloads [a, b) straight into the data file, returning how many bytes were written.
func (d *downloader) stream(ctx context.Context, a, b int64, p *progress) (int64, error) {
	// Abort the request if no bytes arrive for a while (stalled connection).
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stall := time.AfterFunc(60*time.Second, cancel)
	defer stall.Stop()

	body, err := d.r.get(rctx, a, b)
	if err != nil {
		return 0, err
	}
	defer body.Close()
	buf := make([]byte, 256<<10)
	var written int64
	for a+written < b {
		n, err := body.Read(buf[:min(int64(len(buf)), b-a-written)])
		if n > 0 {
			stall.Reset(60 * time.Second)
			if _, werr := d.st.data.WriteAt(buf[:n], a+written); werr != nil {
				return written, fatalError{werr}
			}
			written += int64(n)
			p.add(int64(n))
		}
		if err == io.EOF {
			if a+written < b {
				return written, io.ErrUnexpectedEOF
			}
			break
		}
		if err != nil {
			if rctx.Err() != nil && ctx.Err() == nil {
				return written, errors.New("connection stalled")
			}
			return written, err
		}
	}
	return written, nil
}

func backoff(attempt int, atLeast time.Duration) time.Duration {
	d := time.Second << min(attempt-1, 5) // 1s, 2s, 4s ... 32s
	return max(d, atLeast)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
