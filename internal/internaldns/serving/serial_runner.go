// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"context"
	"strings"
	"sync"
	"time"
)

const daemonCommandLimit = 30 * time.Second

// serialRunner runs daemon commands one at a time in request order, so no two
// console sessions to a daemon overlap. A request identical to the newest
// queued, not yet started command joins it: that run starts after the request
// was made and so applies state at least as new.
type serialRunner struct {
	delegate Runner
	mu       sync.Mutex
	queue    []*serialCall
}

type serialCall struct {
	identity string
	joined   int
	ready    chan struct{}
	done     chan struct{}
	out      []byte
	err      error
}

func newSerialRunner(delegate Runner) *serialRunner {
	if serial, ok := delegate.(*serialRunner); ok {
		return serial
	}
	return &serialRunner{delegate: delegate}
}

func (r *serialRunner) Run(ctx context.Context, c Command) ([]byte, error) {
	identity := c.Path + "\x00" + strings.Join(c.Args, "\x00")
	r.mu.Lock()
	if n := len(r.queue); n > 1 && r.queue[n-1].identity == identity {
		call := r.queue[n-1]
		call.joined++
		r.mu.Unlock()
		select {
		case <-call.done:
			return call.out, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &serialCall{identity: identity, ready: make(chan struct{}), done: make(chan struct{})}
	r.queue = append(r.queue, call)
	if len(r.queue) == 1 {
		close(call.ready)
	}
	r.mu.Unlock()

	select {
	case <-call.ready:
	case <-ctx.Done():
		r.finish(call, nil, ctx.Err())
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		r.finish(call, nil, err)
		return nil, err
	}
	// A started session runs to completion even if the caller is cancelled:
	// abandoning it would let the next command open a second session while
	// the orphaned one is still applying.
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), daemonCommandLimit)
	out, err := r.delegate.Run(runCtx, c)
	cancel()
	r.finish(call, out, err)
	return out, err
}

func (r *serialRunner) finish(call *serialCall, out []byte, err error) {
	r.mu.Lock()
	for i, queued := range r.queue {
		if queued != call {
			continue
		}
		r.queue = append(r.queue[:i], r.queue[i+1:]...)
		if i == 0 && len(r.queue) > 0 {
			close(r.queue[0].ready)
		}
		break
	}
	r.mu.Unlock()
	call.out, call.err = out, err
	close(call.done)
}
