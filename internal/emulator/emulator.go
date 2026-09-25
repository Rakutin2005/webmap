package emulator

import (
	"fmt"
	"hash/fnv"
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
	// MaxAbandoned caps how many emulation runs may be abandoned at the hard
	// deadline before further emulation is disabled. An abandoned run is a VM
	// permanently stuck in native (non-Go, non-interruptible) code — goja's soft
	// Interrupt cannot unwind it, and a Go goroutine cannot be killed, so such
	// runs keep a goroutine + VM + CPU core pinned forever. The only in-process
	// remedy is to stop creating more of them (0 = use the default of 2).
	MaxAbandoned int
}

// HeaderPair is a single header assignment sent with a request.
type HeaderPair struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type NetworkCall struct {
	URL       string       `json:"url"`
	RawURL    string       `json:"rawUrl,omitempty"`
	Method    string       `json:"method"`
	Initiator string       `json:"initiator"`
	Type      string       `json:"type"`
	Body      string       `json:"body,omitempty"`
	Headers   []HeaderPair `json:"headers,omitempty"`
}

// bodyHeaderKey makes the de-dup key sensitive to payloads so distinct request
// bodies to the same endpoint are all kept as separate observations.
func (c NetworkCall) bodyHeaderKey() string {
	if c.Body == "" && len(c.Headers) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, h := range c.Headers {
		sb.WriteString(h.Name)
		sb.WriteString(h.Value)
		sb.WriteString(";")
	}
	return c.Type + "|" + hashShort(c.Body) + "|" + hashShort(sb.String())
}

// hashShort returns a compact stable digest of a payload for de-dup keys.
func hashShort(s string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%x", h.Sum64())
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
	pageHTML    string
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

// SetPageHTML feeds the raw HTML of the emulated page to the sandbox. When
// called before Run, document lookups return real elements built from this
// markup (data-* attributes, meta content, href/src, innerHTML configs become
// readable by page scripts). URL-bearing static attributes are seeded into
// accessors without being recorded — the crawler already harvested them.
func (s *Sandbox) SetPageHTML(html string) { s.pageHTML = html }

// Run executes all scripts in a single realm (so libraries initialize and app
// code can use them), then drains the event loop so deferred and promise-based
// network calls fire.
// abandonedRuns counts emulation runs abandoned at the hard deadline. Such runs
// permanently pin a goroutine and a VM (native code cannot be unwound), so once
// the process-wide cap is reached, further emulation is refused.
var abandonedRuns atomic.Int64

// AbandonedReports returns how many emulation runs have leaked this process.
func AbandonedReports() int64 { return abandonedRuns.Load() }

func (s *Sandbox) Run(scripts []Script) {
	maxAbandoned := s.limits.MaxAbandoned
	if maxAbandoned <= 0 {
		maxAbandoned = 2
	}
	if abandonedRuns.Load() >= int64(maxAbandoned) {
		s.addError(fmt.Sprintf("aborted: emulation disabled after %d native-hang abandonments", maxAbandoned))
		return
	}

	vm, err := newVM(s, s.baseURL)
	if err != nil {
		s.addError(fmt.Sprintf("create vm: %v", err))
		return
	}
	if s.limits.MaxJobs > 0 {
		vm.maxJobs = s.limits.MaxJobs
	}
	vm.buildDOM(s.pageHTML)
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
		abandonedRuns.Add(1)
		s.addError("aborted: hard deadline exceeded (native hang abandoned)")
	}
}

// execAll runs every script in order then drains the event loop. It uses only
// the lock-protected addCall/addError, so it is safe to abandon: if the hard
// deadline fires, this keeps running detached without corrupting shared state.
func (s *Sandbox) execAll(vm *gojaVM, scripts []Script, stopped *int32) {
	runOne := func(scr Script) error {
		// A single malformed script must not abort the whole run.
		defer func() {
			if rec := recover(); rec != nil {
				s.addError(fmt.Sprintf("%s: panic: %v", scr.URL, rec))
			}
		}()
		return vm.exec(scr.Code, scr.URL)
	}
	var deferred []deferredScript
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
		if err := runOne(scr); err != nil {
			// A dependency-style failure (jQuery/$/global not yet defined, e.g.
			// Bitrix plugins before the combined bundle defines jQuery) is worth
			// one more pass once later scripts have run. Report first-pass errors
			// only when the rerun fails too.
			if missingSymbol(err.Error()) {
				deferred = append(deferred, deferredScript{scr: scr, err: err})
			} else {
				s.addError(fmt.Sprintf("%s: %v", scr.URL, err))
			}
		}
	}
	if atomic.LoadInt32(stopped) == 0 {
		s.drain(vm)
	}
	if len(deferred) > 0 && atomic.LoadInt32(stopped) == 0 {
		for _, d := range deferred {
			if err := runOne(d.scr); err != nil {
				s.addError(fmt.Sprintf("%s: %v", d.scr.URL, err))
			}
		}
		s.drain(vm)
	}
}

type deferredScript struct {
	scr Script
	err error
}

// missingSymbol reports whether an execution error is the "global not defined
// yet" family (ReferenceError: X is not defined, TypeError: Value is not an
// object), which one deferred re-run may resolve.
func missingSymbol(msg string) bool {
	return strings.Contains(msg, "is not defined") || strings.Contains(msg, "is not an object")
}

func (s *Sandbox) drain(vm *gojaVM) {
	defer func() {
		if rec := recover(); rec != nil {
			s.addError(fmt.Sprintf("event loop: panic: %v", rec))
		}
	}()
	vm.runLoop()
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
	key := c.Type + "|" + c.Method + "|" + c.URL + "|" + c.bodyHeaderKey()
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
	Abandoned int64
	Disabled  bool
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
		Abandoned: abandonedRuns.Load(),
	}
}
