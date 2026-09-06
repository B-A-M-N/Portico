package supervisor

import "github.com/B-A-M-N/portico/internal/core"

func (p *targetCapturingProvider) capturedTargets() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.targets...)
}

var _ core.Provider = (*targetCapturingProvider)(nil)
