package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var stderrIsTTY = func() bool {
	st, err := os.Stderr.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}()

type sample struct {
	t time.Time
	n int64
}

type progress struct {
	label     string
	total     int64
	done      atomic.Int64
	resumed   int64
	began     time.Time
	quit      chan struct{}
	stopped   sync.WaitGroup
	mu        sync.Mutex
	window    []sample
	lastPrint time.Time
	msg       string
}

func newProgress(label string, total, already int64) *progress {
	p := &progress{label: label, total: total, resumed: already, quit: make(chan struct{})}
	p.done.Store(already)
	return p
}

func (p *progress) add(n int64) { p.done.Add(n) }

// note shows a one-off message (e.g. a retry) above the bar.
func (p *progress) note(msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if stderrIsTTY {
		fmt.Fprintf(os.Stderr, "\r\033[K  %s\n", msg)
	} else {
		fmt.Fprintf(os.Stderr, "%s: %s\n", p.label, msg)
	}
}

func (p *progress) start() {
	p.began = time.Now()
	if p.resumed > 0 {
		fmt.Fprintf(os.Stderr, "%s: resuming, %s of %s already downloaded\n", p.label, fmtBytes(p.resumed), fmtBytes(p.total))
	}
	p.stopped.Add(1)
	go func() {
		defer p.stopped.Done()
		tick := time.NewTicker(200 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-p.quit:
				return
			case <-tick.C:
				p.render(false)
			}
		}
	}()
}

func (p *progress) stop() {
	select {
	case <-p.quit:
	default:
		close(p.quit)
	}
	p.stopped.Wait()
	if stderrIsTTY {
		p.render(true)
		fmt.Fprintln(os.Stderr)
	}
}

func (p *progress) finish() {
	if p.began.IsZero() { // nothing to download
		fmt.Fprintf(os.Stderr, "%s: already downloaded (%s)\n", p.label, fmtBytes(p.total))
		return
	}
	close(p.quit)
	p.stopped.Wait()
	p.render(true)
	el := time.Since(p.began)
	fresh := p.done.Load() - p.resumed
	line := fmt.Sprintf("%s: %s in %s (%s/s)", p.label, fmtBytes(fresh), fmtDur(el), fmtBytes(int64(float64(fresh)/el.Seconds())))
	if stderrIsTTY {
		fmt.Fprintf(os.Stderr, "\r\033[K%s\n", line)
	} else {
		fmt.Fprintln(os.Stderr, line)
	}
}

// speed over the last ~5 seconds, in bytes/sec.
func (p *progress) speed(now time.Time, done int64) float64 {
	p.window = append(p.window, sample{now, done})
	for len(p.window) > 2 && now.Sub(p.window[0].t) > 5*time.Second {
		p.window = p.window[1:]
	}
	first := p.window[0]
	dt := now.Sub(first.t).Seconds()
	if dt < 0.5 {
		// Not enough history yet; fall back to the overall average.
		dt = now.Sub(p.began).Seconds()
		if dt <= 0 {
			return 0
		}
		return float64(done-p.resumed) / dt
	}
	return float64(done-first.n) / dt
}

func (p *progress) render(final bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	done := p.done.Load()
	sp := p.speed(now, done)
	frac := 0.0
	if p.total > 0 {
		frac = float64(done) / float64(p.total)
	}
	eta := "--:--"
	if sp > 0 && done < p.total {
		eta = fmtDur(time.Duration(float64(p.total-done) / sp * float64(time.Second)))
	} else if done >= p.total {
		eta = "0:00"
	}

	if !stderrIsTTY {
		// Logs: one line every 5 seconds.
		if !final && now.Sub(p.lastPrint) < 5*time.Second {
			return
		}
		p.lastPrint = now
		fmt.Fprintf(os.Stderr, "%s: %5.1f%%  %s / %s  %s/s  ETA %s\n",
			p.label, frac*100, fmtBytes(done), fmtBytes(p.total), fmtBytes(int64(sp)), eta)
		return
	}

	const width = 28
	filled := int(frac * width * 8) // eighths, for smooth partial blocks
	var bar strings.Builder
	for i := 0; i < width; i++ {
		switch cell := filled - i*8; {
		case cell >= 8:
			bar.WriteString("█")
		case cell <= 0:
			bar.WriteString("\033[2m░\033[22m")
		default:
			bar.WriteString(string([]rune("▏▎▍▌▋▊▉")[cell-1]))
		}
	}
	fmt.Fprintf(os.Stderr, "\r\033[K\033[1m%-6s\033[22m %s \033[1m%5.1f%%\033[22m  %s / %s  \033[36m%s/s\033[39m  ETA %s",
		p.label, bar.String(), frac*100, fmtBytes(done), fmtBytes(p.total), fmtBytes(int64(sp)), eta)
}

func fmtBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	f := float64(n)
	for _, u := range []string{"kB", "MB", "GB", "TB"} {
		f /= unit
		if f < unit {
			return fmt.Sprintf("%.1f %s", f, u)
		}
	}
	return fmt.Sprintf("%.1f PB", f/unit)
}

func fmtDur(d time.Duration) string {
	s := int(d.Round(time.Second).Seconds())
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}
