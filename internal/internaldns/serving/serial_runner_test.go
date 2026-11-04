// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

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
		joined := r.queue[len(r.queue)-1].waiters - 1
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

func TestSerialRunnerHandsACancelledQueuedCallToAJoiner(t *testing.T) {
	t.Parallel()
	console := newFakeConsole()
	runner := newSerialRunner(console)
	first := make(chan error, 1)
	go func() {
		_, err := runner.Run(context.Background(), reloadNode)
		first <- err
	}()
	waitStarted(t, console)
	ownerCtx, cancelOwner := context.WithCancel(context.Background())
	owner := make(chan error, 1)
	go func() {
		_, err := runner.Run(ownerCtx, gateNode)
		owner <- err
	}()
	waitQueued(t, runner, 2)
	joiner := make(chan error, 1)
	go func() {
		_, err := runner.Run(context.Background(), gateNode)
		joiner <- err
	}()
	waitJoined(t, runner, 1)
	cancelOwner()
	if err := <-owner; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled owner returned %v", err)
	}
	console.release <- struct{}{}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if name := waitStarted(t, console); name != daemonName(gateNode) {
		t.Fatalf("started %q, want the joined gate", name)
	}
	console.release <- struct{}{}
	if err := <-joiner; err != nil {
		t.Fatalf("joiner lost its update with its owner: %v", err)
	}
	if _, pushes := console.snapshot(); len(pushes) != 2 {
		t.Fatalf("pushes = %v, want the reload and one gate", pushes)
	}
}

func TestSerialRunnerDropsAQueuedCallEveryCallerCancelled(t *testing.T) {
	t.Parallel()
	console := newFakeConsole()
	runner := newSerialRunner(console)
	first := make(chan error, 1)
	go func() {
		_, err := runner.Run(context.Background(), reloadNode)
		first <- err
	}()
	waitStarted(t, console)
	ctx, cancel := context.WithCancel(context.Background())
	abandoned := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := runner.Run(ctx, gateNode)
			abandoned <- err
		}()
	}
	waitQueued(t, runner, 2)
	waitJoined(t, runner, 1)
	cancel()
	for range 2 {
		if err := <-abandoned; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled caller returned %v", err)
		}
	}
	waitQueued(t, runner, 1)
	later := make(chan error, 1)
	go func() {
		_, err := runner.Run(context.Background(), reloadNode)
		later <- err
	}()
	waitQueued(t, runner, 2)
	console.release <- struct{}{}
	if name := waitStarted(t, console); name != daemonName(reloadNode) {
		t.Fatalf("started %q after every caller of the gate left", name)
	}
	console.release <- struct{}{}
	for _, done := range []chan error{first, later} {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestExecRunnerKillsTheWholeCommandOnCancellation(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "child")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := ExecRunner{}.Run(ctx, Command{Path: "/bin/sh", Args: []string{"-c", `sleep 30 & echo $! > "$0"; wait`, pidFile}})
	if err == nil {
		t.Fatal("cancelled command reported success")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("Run returned after %s, not at cancellation", elapsed)
	}
	data, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
	if convErr != nil {
		t.Fatal(convErr)
	}
	deadline := time.Now().Add(5 * time.Second)
	for unix.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("child %d of a cancelled command is still running", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestUpdateSummaryCountsOnlyRuleStatements(t *testing.T) {
	t.Parallel()
	data := []byte("rules={}\n-- local function appendRule(rule,action) rules[#rules+1] = newRuleAction(rule,action) end\n" +
		"local function appendRule(rule,action) rules[#rules+1] = newRuleAction(rule,action) end\n" +
		"appendRule(AllRule(),PoolAction(\"a\"))\n  appendRule(AllRule(),PoolAction(\"b\"))\n-- appendRule(AllRule(),DropAction())\n")
	if got := summarizeUpdate("dnsdist.conf", data).Rules; got != 2 {
		t.Fatalf("Rules = %d, want 2", got)
	}
}

func TestExecRunnerKillsChildrenLeftByACompletedCommand(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "child")
	out, err := ExecRunner{}.Run(context.Background(), Command{Path: "/bin/sh", Args: []string{"-c", `sleep 30 >/dev/null 2>&1 & echo $! > "$0"; echo applied`, pidFile}})
	if err != nil || strings.TrimSpace(string(out)) != "applied" {
		t.Fatalf("Run() = %q, %v", out, err)
	}
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for unix.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("child %d outlived its completed command", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
