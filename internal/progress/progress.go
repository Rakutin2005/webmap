package progress

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type ProgressBar struct {
	target    int64
	completed int64
	requests  int64
	width     int
	mu        sync.Mutex
	running   bool
	stopChan  chan struct{}
	lastLine  string
}

func New(target int) *ProgressBar {
	return &ProgressBar{
		target: int64(target),
		width:  40,
	}
}

func (p *ProgressBar) AddTarget(delta int) {
	if delta > 0 {
		atomic.AddInt64(&p.target, int64(delta))
	}
}

func (p *ProgressBar) Start(done chan struct{}) {
	p.running = true
	p.stopChan = make(chan struct{})
	go p.drawLoop(done)
}

func (p *ProgressBar) Stop() {
	if p.running && p.stopChan != nil {
		close(p.stopChan)
	}
	p.running = false
}

func (p *ProgressBar) Increment() {
	atomic.AddInt64(&p.completed, 1)
}

func (p *ProgressBar) IncrementRequest() {
	atomic.AddInt64(&p.requests, 1)
}

func (p *ProgressBar) Completed() int {
	return int(atomic.LoadInt64(&p.completed))
}

func (p *ProgressBar) Target() int {
	return int(atomic.LoadInt64(&p.target))
}

func (p *ProgressBar) Requests() int {
	return int(atomic.LoadInt64(&p.requests))
}

func (p *ProgressBar) drawLoop(done chan struct{}) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			p.drawOnce()
			fmt.Fprintln(os.Stderr)
			return
		case <-p.stopChan:
			return
		case <-ticker.C:
			p.drawOnce()
		}
	}
}

func (p *ProgressBar) drawOnce() {
	completed := atomic.LoadInt64(&p.completed)
	target := atomic.LoadInt64(&p.target)
	requests := atomic.LoadInt64(&p.requests)

	if target == 0 {
		target = 1
	}

	percent := float64(completed) / float64(target)
	if percent > 1.0 {
		percent = 1.0
	}
	if percent < 0 {
		percent = 0
	}

	filled := int(float64(p.width) * percent)
	empty := p.width - filled

	bar := strings.Repeat("#", filled) + strings.Repeat("-", empty)

	p.mu.Lock()
	defer p.mu.Unlock()

	output := fmt.Sprintf("\r[%s] %d/%d (%.0f%%) reqs=%d", bar, completed, target, percent*100, requests)

	if output == p.lastLine {
		return
	}
	p.lastLine = output

	fmt.Fprint(os.Stderr, output)
}

func (p *ProgressBar) Finish() {
	completed := atomic.LoadInt64(&p.completed)
	target := atomic.LoadInt64(&p.target)
	requests := atomic.LoadInt64(&p.requests)

	fmt.Fprintf(os.Stderr, "\r[%s] %d/%d (100%%) reqs=%d\n", strings.Repeat("#", p.width), completed, target, requests)
}
