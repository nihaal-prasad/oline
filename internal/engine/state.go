package engine

import (
	"sync"

	"github.com/nihaal-prasad/oline/internal/core"
)

// stateMachine tracks an agent's lifecycle and provides the channel the agent
// loop blocks on while paused. Pause/Resume/Kill are safe to call from any
// goroutine (e.g. the TUI control thread) so pause can take effect immediately,
// out of band from the mailbox.
type stateMachine struct {
	mu     sync.Mutex
	st     core.State
	resume chan struct{} // closed on Resume (or Kill) to wake a waiting gate
}

func newStateMachine() *stateMachine {
	return &stateMachine{st: core.Running, resume: make(chan struct{})}
}

// snapshot returns the current state and the channel to wait on if paused.
func (s *stateMachine) snapshot() (core.State, chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st, s.resume
}

func (s *stateMachine) State() core.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st
}

func (s *stateMachine) pause() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st == core.Killed || s.st == core.Paused {
		return false
	}
	s.st = core.Paused
	return true
}

func (s *stateMachine) resumeNow() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st != core.Paused {
		return false
	}
	s.st = core.Running
	close(s.resume)
	s.resume = make(chan struct{})
	return true
}

func (s *stateMachine) setIdle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st == core.Running {
		s.st = core.Idle
	}
}

func (s *stateMachine) setRunning() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st == core.Idle {
		s.st = core.Running
	}
}

func (s *stateMachine) kill() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st == core.Killed {
		return
	}
	s.st = core.Killed
	close(s.resume) // wake any gate; it will observe Killed and return
}
