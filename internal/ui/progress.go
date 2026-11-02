package ui

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

// Status is a one-line progress display on stderr for a long step: a
// spinner, the step, a bar when the total is known, and the elapsed time,
// redrawn in place. It shows in text mode on a terminal only, and not with
// --verbose, where Terraform's own output says the same. A nil *Status is
// valid and does nothing.
type Status struct {
	mu          sync.Mutex
	label, verb string
	done, total int
	start       time.Time
	frame       int
	stop        chan struct{}
	stopped     chan struct{}
}

func statusEnabled() bool {
	return !JSON && !Verbose && term.IsTerminal(int(os.Stderr.Fd()))
}

// Begin starts a status line, e.g. Begin("network", "applying", 40). With
// total 0 there is no bar.
func Begin(label, verb string, total int) *Status {
	if !statusEnabled() {
		return nil
	}
	s := &Status{label: label, verb: verb, total: total, start: time.Now(),
		stop: make(chan struct{}), stopped: make(chan struct{})}
	go s.loop()
	return s
}

func (s *Status) loop() {
	defer close(s.stopped)
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		s.draw()
		select {
		case <-s.stop:
			return
		case <-t.C:
		}
	}
}

var spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func (s *Status) line(mark string) string {
	b := &strings.Builder{}
	fmt.Fprintf(b, "%s %-14s %s", mark, s.label, s.verb)
	if s.total > 0 {
		const width = 24
		n := min(s.done, s.total) * width / s.total
		fmt.Fprintf(b, "  %s%s  %d/%d", strings.Repeat("█", n), strings.Repeat("░", width-n), min(s.done, s.total), s.total)
	}
	d := time.Since(s.start).Round(time.Second)
	fmt.Fprintf(b, "  %dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	return b.String()
}

func (s *Status) draw() {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprint(os.Stderr, "\r\033[K"+s.line(spinner[s.frame%len(spinner)]))
	s.frame++
}

// Step counts one finished unit of work toward the total.
func (s *Status) Step() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.done++
	s.mu.Unlock()
}

// Set replaces the progress, for work whose total can change, such as nodes
// that are still being listed.
func (s *Status) Set(done, total int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.done, s.total = done, total
	s.mu.Unlock()
}

// End stops the status line. With a non-empty verb the line is kept, e.g.
// "✓ network applied ..."; otherwise it is erased. ok picks the mark.
func (s *Status) End(verb string, ok bool) {
	if s == nil {
		return
	}
	close(s.stop)
	<-s.stopped
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprint(os.Stderr, "\r\033[K")
	if verb == "" {
		return
	}
	s.verb = verb
	mark := "✓"
	if !ok {
		mark = "✗"
	}
	fmt.Fprintln(os.Stderr, s.line(mark))
}
