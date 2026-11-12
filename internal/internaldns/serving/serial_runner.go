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
// console sessions to a daemon overlap. One queue covers every daemon on the
// member: gate commands name no daemon, and a transaction reloads its daemons
// in turn anyway. A stuck session holds the queue for at most
// daemonCommandLimit. A request identical to the newest queued, not yet
// started command joins it: that run starts after the request was made and so
// applies state at least as new.
type serialRunner struct {
	delegate Runner
	mu       sync.Mutex
	queue    []*serialCall
}

// serialCall runs once, by whichever of its waiters is still waiting when it
// reaches the head of its queue, so a cancelled caller does not take
// identical callers down with it.
type serialCall struct {
	identity string
	waiters  int
	running  bool
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
	var call *serialCall
	if n := len(r.queue); n > 1 && r.queue[n-1].identity == identity {
		call = r.queue[n-1]
	} else {
		call = &serialCall{identity: identity, ready: make(chan struct{}), done: make(chan struct{})}
		r.queue = append(r.queue, call)
		if len(r.queue) == 1 {
			close(call.ready)
		}
	}
	call.waiters++
	r.mu.Unlock()

	select {
	case <-call.ready:
	case <-call.done:
		return call.out, call.err
	case <-ctx.Done():
		r.leave(call)
		return nil, ctx.Err()
	}
	r.mu.Lock()
	if call.running || ctx.Err() != nil {
		r.mu.Unlock()
		select {
		case <-call.done:
			return call.out, call.err
		case <-ctx.Done():
			r.leave(call)
			return nil, ctx.Err()
		}
	}
	call.running = true
	r.mu.Unlock()
	// A started session runs to completion even if its callers are cancelled,
	// up to daemonCommandLimit. ExecRunner then kills the command's whole
	// process group and waits for it before the next session starts.
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), daemonCommandLimit)
	out, err := r.delegate.Run(runCtx, c)
	cancel()
	r.finish(call, out, err)
	return out, err
}

// leave drops a cancelled waiter. The last waiter of a call that has not
// started removes it from the queue.
func (r *serialRunner) leave(call *serialCall) {
	r.mu.Lock()
	call.waiters--
	abandoned := call.waiters == 0 && !call.running && r.removeLocked(call)
	r.mu.Unlock()
	if abandoned {
		close(call.done)
	}
}

func (r *serialRunner) finish(call *serialCall, out []byte, err error) {
	r.mu.Lock()
	r.removeLocked(call)
	r.mu.Unlock()
	call.out, call.err = out, err
	close(call.done)
}

// removeLocked removes call from the queue and starts the next one. It
// reports whether call was still queued.
func (r *serialRunner) removeLocked(call *serialCall) bool {
	for i, queued := range r.queue {
		if queued != call {
			continue
		}
		r.queue = append(r.queue[:i], r.queue[i+1:]...)
		if i == 0 && len(r.queue) > 0 {
			close(r.queue[0].ready)
		}
		return true
	}
	return false
}
