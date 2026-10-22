// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
)

type fakeConsole struct {
	mu      sync.Mutex
	active  int
	overlap bool
	pushes  []string
	started chan string
	release chan struct{}
	ctxErrs []error
}

func newFakeConsole() *fakeConsole {
	return &fakeConsole{started: make(chan string, 64), release: make(chan struct{})}
}

func (f *fakeConsole) Run(ctx context.Context, c Command) ([]byte, error) {
	name := daemonName(c)
	f.mu.Lock()
	f.active++
	if f.active > 1 {
		f.overlap = true
	}
	f.pushes = append(f.pushes, name)
	f.mu.Unlock()
	f.started <- name
	<-f.release
	f.mu.Lock()
	f.active--
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	f.mu.Unlock()
	return nil, nil
}

func (f *fakeConsole) snapshot() (bool, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.overlap, append([]string(nil), f.pushes...)
}

func waitStarted(t *testing.T, f *fakeConsole) string {
	t.Helper()
	select {
	case name := <-f.started:
		return name
	case <-time.After(5 * time.Second):
		t.Fatal("console session did not start")
		return ""
	}
}

func waitQueued(t *testing.T, r *serialRunner, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		queued := len(r.queue)
		r.mu.Unlock()
		if queued == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("queue never reached %d commands", n)
}

func waitJoined(t *testing.T, r *serialRunner, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		joined := r.queue[len(r.queue)-1].joined
		r.mu.Unlock()
		if joined == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("queued command never gained %d joined requests", n)
}

var (
	reloadNode = Command{Path: "/usr/local/sbin/internal-dns-reload-dnsdist", Args: []string{"node"}}
	gateNode   = Command{Path: "/usr/local/sbin/internal-dns-gate", Args: []string{"node"}}
)

func TestSerialRunnerNeverOverlapsAndCoalescesQueuedUpdates(t *testing.T) {
	t.Parallel()
	console := newFakeConsole()
	runner := newSerialRunner(console)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	run := func() {
		defer wg.Done()
		_, err := runner.Run(context.Background(), reloadNode)
		errs <- err
	}
	wg.Add(1)
	go run()
	waitStarted(t, console)
	for range 5 {
		wg.Add(1)
		go run()
	}
	waitQueued(t, runner, 2)
	waitJoined(t, runner, 4)
	console.release <- struct{}{}
	waitStarted(t, console)
	console.release <- struct{}{}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	overlap, pushes := console.snapshot()
	if overlap {
		t.Fatal("two console sessions to one daemon overlapped")
	}
	if len(pushes) != 2 {
		t.Fatalf("pushes = %d, want the in-flight push plus one coalesced push: %v", len(pushes), pushes)
	}
}

func TestSerialRunnerKeepsOrderAcrossDifferentCommands(t *testing.T) {
	t.Parallel()
	console := newFakeConsole()
	runner := newSerialRunner(console)
	var wg sync.WaitGroup
	start := func(c Command, queued int) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := runner.Run(context.Background(), c); err != nil {
				t.Error(err)
			}
		}()
		waitQueued(t, runner, queued)
	}
	start(reloadNode, 1)
	waitStarted(t, console)
	start(gateNode, 2)
	start(reloadNode, 3)
	console.release <- struct{}{}
	waitStarted(t, console)
	console.release <- struct{}{}
	waitStarted(t, console)
	console.release <- struct{}{}
	wg.Wait()
	overlap, pushes := console.snapshot()
	want := []string{daemonName(reloadNode), daemonName(gateNode), daemonName(reloadNode)}
	if overlap || strings.Join(pushes, ",") != strings.Join(want, ",") {
		t.Fatalf("overlap=%t pushes=%v, want %v: a reload requested after a gate must not merge into an earlier reload", overlap, pushes, want)
	}
}

func TestSerialRunnerFinishesStartedSessionAfterCancellation(t *testing.T) {
	t.Parallel()
	console := newFakeConsole()
	runner := newSerialRunner(console)
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() {
		_, err := runner.Run(ctx, reloadNode)
		first <- err
	}()
	waitStarted(t, console)
	cancel()
	second := make(chan error, 1)
	go func() {
		_, err := runner.Run(context.Background(), gateNode)
		second <- err
	}()
	waitQueued(t, runner, 2)
	select {
	case <-console.started:
		t.Fatal("a second session started while a cancelled one was still applying")
	case <-time.After(20 * time.Millisecond):
	}
	console.release <- struct{}{}
	waitStarted(t, console)
	console.release <- struct{}{}
	if err := <-first; err != nil {
		t.Fatalf("started session reported %v", err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	console.mu.Lock()
	defer console.mu.Unlock()
	if console.overlap || console.ctxErrs[0] != nil {
		t.Fatalf("overlap=%t session context=%v", console.overlap, console.ctxErrs[0])
	}
	if _, err := runner.Run(ctx, reloadNode); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled caller started a session: %v", err)
	}
}

func TestReloadLogsUpdateSummaryWithoutPayloadByDefault(t *testing.T) {
	t.Parallel()
	payload := "local rules = {}\nlocal function appendRule(rule,action) end\nappendRule(AllRule(),PoolAction(\"p\"))\nsetRules(rules)\n"
	for _, verbosity := range []int{0, 1} {
		dir := t.TempDir()
		final := filepath.Join(dir, "live", "node-dnsdist.conf")
		var mu sync.Mutex
		var lines []string
		logger := funcr.New(func(prefix, args string) {
			mu.Lock()
			lines = append(lines, prefix+" "+args)
			mu.Unlock()
		}, funcr.Options{Verbosity: verbosity})
		reload := Command{Path: "/usr/local/sbin/internal-dns-reload-dnsdist", Args: []string{"node"}, WhenChanged: []string{final}}
		tx := FileTransaction{Config: TransactionConfig{StageDir: dir, Reload: []Command{reload}}, Runner: &recordingRunner{}}
		if err := tx.Apply(logr.NewContext(context.Background(), logger), map[string][]byte{final: []byte(payload)}); err != nil {
			t.Fatal(err)
		}
		summary := summarizeUpdate(final, []byte(payload))
		joined := strings.Join(lines, "\n")
		for _, want := range []string{"sending daemon update", "internal-dns-reload-dnsdist node", summary.SHA256, "daemon update applied"} {
			if !strings.Contains(joined, want) {
				t.Fatalf("verbosity %d log lacks %q:\n%s", verbosity, want, joined)
			}
		}
		if summary.Rules != 1 || summary.Bytes != len(payload) {
			t.Fatalf("summary = %+v", summary)
		}
		if logged := strings.Contains(joined, "PoolAction"); logged != (verbosity > 0) {
			t.Fatalf("verbosity %d logged payload=%t:\n%s", verbosity, logged, joined)
		}
	}
}
