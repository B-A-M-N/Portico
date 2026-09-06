package supervisor

import (
	"fmt"
	"sync"
)

// mutationGate keeps shutdown from closing durable dependencies while an
// admitted handler is still using them. It is intentionally separate from the
// supervisor's ready flag: readiness describes serving, while this gate owns
// mutation admission and teardown ordering.
type mutationGate struct {
	mu     sync.Mutex
	cond   *sync.Cond
	open   bool
	leases int
}

func (s *Supervisor) gate() *mutationGate {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mutation == nil {
		s.mutation = &mutationGate{open: s.mutating}
		s.mutation.cond = sync.NewCond(&s.mutation.mu)
	}
	return s.mutation
}

// admitMutation reserves the supervisor's durable dependencies for one
// handler. The returned release is idempotent so error paths cannot leak a
// lease and hold shutdown forever.
func (s *Supervisor) admitMutation() (func(), error) {
	g := s.gate()
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.open {
		return nil, fmt.Errorf("supervisor is shutting down and not accepting mutations")
	}
	g.leases++
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.leases--
			if g.leases == 0 {
				g.cond.Broadcast()
			}
			g.mu.Unlock()
		})
	}, nil
}

// beginShutdown closes admission and waits for already-admitted handlers.
func (s *Supervisor) beginShutdown() {
	g := s.gate()
	g.mu.Lock()
	g.open = false
	s.mu.Lock()
	s.mutating = false
	s.mu.Unlock()
	for g.leases > 0 {
		g.cond.Wait()
	}
	g.mu.Unlock()
}

// AcceptingMutations reports whether new durable mutation handlers may enter.
func (s *Supervisor) AcceptingMutations() bool {
	g := s.gate()
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.open
}
