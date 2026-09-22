package emulator

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Limits bounds the resources a single page emulation may consume.
type Limits struct {
	Timeout time.Duration // total JS execution budget per page (0 = unlimited)
	MaxJS   int           // skip scripts larger than this many bytes (0 = unlimited)
	MaxJobs int           // event-loop job cap (0 = default)
}

type NetworkCall struct {
	URL       string `json:"url"`
	RawURL    string `json:"rawUrl,omitempty"`
	Method    string `json:"method"`
	Initiator string `json:"initiator"`
	Type      string `json:"type"`
}

type Script struct {
	Code string
	URL  string
}

type Sandbox struct {
	mu          sync.Mutex
	calls       []NetworkCall
	seen        map[string]bool
	vm          *gojaVM
	baseURL     string
	limits      Limits
	ScriptCount int
	Errors      []string
}

// New creates a sandbox that resolves discovered URLs against baseURL (the page
// being emulated). Pass "" if no base is known.
func New(baseURL string) *Sandbox {
	return &Sandbox{baseURL: baseURL, seen: map[string]bool{}}
}

// NewWithLimits creates a sandbox with explicit resource limits.
func NewWithLimits(baseURL string, limits Limits) *Sandbox {
	return &Sandbox{baseURL: baseURL, seen: map[string]bool{}, limits: limits}
}

// Run executes all scripts in a single realm (so libraries initialize and app
// code can use them), then drains the event loop so deferred and promise-based
// network calls fire.
func (s *Sandbox) Run(scripts []Script) {
	vm, err := newVM(s, s.baseURL)
	if err != nil {
		s.addError(fmt.Sprintf("create vm: %v", err))
		return
	}
	if s.limits.MaxJobs > 0 {
		vm.maxJobs = s.limits.MaxJobs
	}
	s.mu.Lock()
	s.vm = vm
	s.ScriptCount = len(scripts)
	s.mu.Unlock()

	var stopped int32
	if s.limits.Timeout > 0 {
		vm.deadline = time.Now().Add(s.limits.Timeout)
		vm.stopped = &stopped
	}

	// Execute in a goroutine so a hang cannot block the caller. goja.Interrupt
	// (the soft stop below) unwinds pure-JS loops, but it CANNOT interrupt native
	// Go work — regexp2 catastrophic backtracking, a stuck syscall — so a hard
	// wall-clock deadline guarantees Run always returns and the crawl pipeline
	// never deadlocks.
	done := make(chan struct{})
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				s.addError(fmt.Sprintf("emulation: panic: %v", rec))
			}
			close(done)
		}()
		s.execAll(vm, scripts, &stopped)
	}()

	if s.limits.Timeout <= 0 {
		<-done
		return
	}

	soft := time.AfterFunc(s.limits.Timeout, func() {
		atomic.StoreInt32(&stopped, 1)
		vm.runtime.Interrupt("emulation time budget exceeded")
	})
	defer soft.Stop()

	hard := s.limits.Timeout + 3*time.Second
	select {
	case <-done:
	case <-time.After(hard):
		atomic.StoreInt32(&stopped, 1)
		vm.runtime.Interrupt("emulation hard deadline")
		s.addError("aborted: hard deadline exceeded (native hang abandoned)")
	}
}

// execAll runs every script in order then drains the event loop. It uses only
// the lock-protected addCall/addError, so it is safe to abandon: if the hard
// deadline fires, this keeps running detached without corrupting shared state.
func (s *Sandbox) execAll(vm *gojaVM, scripts []Script, stopped *int32) {
	maxBytes := s.limits.MaxJS
	for _, scr := range scripts {
		if atomic.LoadInt32(stopped) == 1 {
			s.addError("aborted: time budget exceeded")
			return
		}
		if maxBytes > 0 && len(scr.Code) > maxBytes {
			s.addError(fmt.Sprintf("%s: skipped (%d bytes > limit)", scr.URL, len(scr.Code)))
			continue
		}
		func() {
			// A single malformed script must not abort the whole run.
			defer func() {
				if rec := recover(); rec != nil {
					s.addError(fmt.Sprintf("%s: panic: %v", scr.URL, rec))
				}
			}()
			if err := vm.exec(scr.Code, scr.URL); err != nil {
				s.addError(fmt.Sprintf("%s: %v", scr.URL, err))
			}
		}()
	}
	if atomic.LoadInt32(stopped) == 0 {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					s.addError(fmt.Sprintf("event loop: panic: %v", rec))
				}
			}()
			vm.runLoop()
		}()
	}
}

func (s *Sandbox) addError(msg string) {
	s.mu.Lock()
	s.Errors = append(s.Errors, msg)
	s.mu.Unlock()
}

func (s *Sandbox) Calls() []NetworkCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]NetworkCall, len(s.calls))
	copy(out, s.calls)
	return out
}

func (s *Sandbox) addCall(c NetworkCall) {
	key := c.Type + "|" + c.Method + "|" + c.URL
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen[key] {
		return
	}
	s.seen[key] = true
	s.calls = append(s.calls, c)
}

func (s *Sandbox) Stats() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("  Scripts executed: %d\n", s.ScriptCount))
	sb.WriteString(fmt.Sprintf("  Network calls intercepted: %d\n", len(s.calls)))
	byType := map[string]int{}
	for _, c := range s.calls {
		byType[c.Type]++
	}
	for _, t := range []string{"fetch", "xhr", "websocket", "beacon", "image", "script", "other"} {
		if n := byType[t]; n > 0 {
			sb.WriteString(fmt.Sprintf("    %s: %d\n", t, n))
		}
	}
	if len(s.Errors) > 0 {
		sb.WriteString(fmt.Sprintf("  Errors: %d\n", len(s.Errors)))
		for _, e := range s.Errors {
			sb.WriteString(fmt.Sprintf("    %s\n", e))
		}
	}
	return sb.String()
}

type Summary struct {
	Scripts   int
	Calls     int
	ByType    map[string]int
	Errors    int
	ErrorList []string
	List      []NetworkCall
}

func (s *Summary) LoadErrors() []string { return s.ErrorList }

func (s *Sandbox) Summary() Summary {
	s.mu.Lock()
	defer s.mu.Unlock()

	byType := map[string]int{}
	for _, c := range s.calls {
		byType[c.Type]++
	}
	out := make([]NetworkCall, len(s.calls))
	copy(out, s.calls)

	errList := make([]string, len(s.Errors))
	copy(errList, s.Errors)

	return Summary{
		Scripts:   s.ScriptCount,
		Calls:     len(s.calls),
		ByType:    byType,
		Errors:    len(s.Errors),
		ErrorList: errList,
		List:      out,
	}
}
