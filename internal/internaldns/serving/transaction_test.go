package serving

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type recordingRunner struct {
	calls  []Command
	failAt int
}

type readableConfigRunner struct{ t *testing.T }

func (r readableConfigRunner) Run(_ context.Context, c Command) ([]byte, error) {
	stageInfo, err := os.Stat(filepath.Dir(c.Args[0]))
	if err != nil {
		return nil, err
	}
	fileInfo, err := os.Stat(c.Args[0])
	if err != nil {
		return nil, err
	}
	if stageInfo.Mode().Perm()&0o005 != 0o005 || fileInfo.Mode().Perm()&0o004 != 0o004 {
		r.t.Fatalf("staged config is not service-readable: dir=%#o file=%#o", stageInfo.Mode().Perm(), fileInfo.Mode().Perm())
	}
	return nil, nil
}

func (r *recordingRunner) Run(_ context.Context, c Command) ([]byte, error) {
	r.calls = append(r.calls, c)
	if r.failAt == len(r.calls) {
		return []byte("bad config"), errors.New("exit 1")
	}
	return nil, nil
}

func TestFileTransactionValidatesBeforeInstall(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	final := filepath.Join(dir, "live", "named.conf")
	runner := &recordingRunner{failAt: 1}
	tx := FileTransaction{Config: TransactionConfig{StageDir: dir, Validate: []Command{{Path: "check", Args: []string{"{stageDir}/named.conf"}}}}, Runner: runner}
	if err := tx.Apply(context.Background(), map[string][]byte{final: []byte("new")}); err == nil {
		t.Fatal("invalid config installed")
	}
	if _, err := os.Stat(final); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("final file exists: %v", err)
	}
	if got := runner.calls[0].Args[0]; filepath.Base(filepath.Dir(got)) == "live" {
		t.Fatalf("validator saw final path %q", got)
	}
}

func TestFileTransactionConfigIsReadableByServiceUID(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	final := filepath.Join(dir, "runtime", "dnsdist.conf")
	tx := FileTransaction{
		Config: TransactionConfig{StageDir: dir, Validate: []Command{{Path: "check", Args: []string{"{stageDir}/dnsdist.conf"}}}},
		Runner: readableConfigRunner{t: t},
	}
	if err := tx.Apply(context.Background(), map[string][]byte{final: []byte("setLocal('127.0.0.1:53')")}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(final)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("installed mode = %#o, want 0644", info.Mode().Perm())
	}
}

func TestFileTransactionPreservesStagePlaceholderAcrossApplies(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	final := filepath.Join(dir, "runtime", "dnsdist.conf")
	runner := &recordingRunner{}
	tx := FileTransaction{
		Config: TransactionConfig{StageDir: dir, Validate: []Command{{Path: "check", Args: []string{"{stageDir}/dnsdist.conf"}}}},
		Runner: runner,
	}
	if err := tx.Apply(context.Background(), map[string][]byte{final: []byte("revision one")}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Apply(context.Background(), map[string][]byte{final: []byte("revision two")}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("validator calls = %d, want 2", len(runner.calls))
	}
	first, second := runner.calls[0].Args[0], runner.calls[1].Args[0]
	if first == second {
		t.Fatalf("successive transactions reused staging path %q", first)
	}
	if tx.Config.Validate[0].Args[0] != "{stageDir}/dnsdist.conf" {
		t.Fatalf("transaction config mutated to %q", tx.Config.Validate[0].Args[0])
	}
}

func TestFileTransactionForceReloadsUnchangedFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	final := filepath.Join(dir, "runtime", "dnsdist.conf")
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(final, []byte("unchanged"), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}
	tx := FileTransaction{Config: TransactionConfig{
		StageDir: dir,
		Validate: []Command{{Path: "validate", Args: []string{"{stageDir}/dnsdist.conf"}, WhenChanged: []string{final}}},
		Reload:   []Command{{Path: "reload", WhenChanged: []string{final}}},
	}, Runner: runner}
	if err := tx.ApplyForce(context.Background(), map[string][]byte{final: []byte("unchanged")}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 || runner.calls[0].Path != "validate" || runner.calls[1].Path != "reload" {
		t.Fatalf("forced activation calls = %#v", runner.calls)
	}
}

func TestFileTransactionRollsBackFailedReload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	final := filepath.Join(dir, "named.conf")
	if err := os.WriteFile(final, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{failAt: 2}
	tx := FileTransaction{Config: TransactionConfig{StageDir: dir, Validate: []Command{{Path: "check"}}, Reload: []Command{{Path: "reload"}}}, Runner: runner}
	if err := tx.Apply(context.Background(), map[string][]byte{final: []byte("new")}); err == nil {
		t.Fatal("reload failure accepted")
	}
	got, err := os.ReadFile(final)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old" {
		t.Fatalf("rollback got %q", got)
	}
	if len(runner.calls) != 3 {
		t.Fatalf("got %d commands, want validate + reload + rollback reload", len(runner.calls))
	}
}

func TestFileTransactionReloadsOnlyChangedTier(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dnsdist := filepath.Join(dir, "dnsdist.conf")
	named := filepath.Join(dir, "named.conf")
	if err := os.WriteFile(dnsdist, []byte("old deadline"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(named, []byte("unchanged"), 0o640); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}
	tx := FileTransaction{Config: TransactionConfig{
		StageDir: dir,
		Validate: []Command{
			{Path: "validate-dnsdist", Args: []string{"{stageDir}/dnsdist.conf"}, WhenChanged: []string{dnsdist}},
			{Path: "validate-bind", Args: []string{"{stageDir}/named.conf"}, WhenChanged: []string{named}},
		},
		Reload: []Command{
			{Path: "reload-dnsdist", WhenChanged: []string{dnsdist}},
			{Path: "reload-bind", WhenChanged: []string{named}},
		},
	}, Runner: runner}
	if err := tx.Apply(context.Background(), map[string][]byte{dnsdist: []byte("new deadline"), named: []byte("unchanged")}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 || runner.calls[0].Path != "validate-dnsdist" || runner.calls[1].Path != "reload-dnsdist" {
		t.Fatalf("reload calls = %#v", runner.calls)
	}
	if got, err := os.ReadFile(named); err != nil || string(got) != "unchanged" {
		t.Fatalf("unchanged BIND file was disturbed: %q, %v", got, err)
	}
}

type transactionRunnerFunc func(context.Context, Command) ([]byte, error)

func (f transactionRunnerFunc) Run(ctx context.Context, c Command) ([]byte, error) { return f(ctx, c) }

func TestFileTransactionValidatesFreshStagedZoneReferences(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	config := filepath.Join(dir, "live", "named.conf")
	zone := filepath.Join(dir, "live", "zone.db")
	runner := transactionRunnerFunc(func(_ context.Context, c Command) ([]byte, error) {
		data, err := os.ReadFile(c.Args[0])
		if err != nil {
			return nil, err
		}
		staged := filepath.Join(filepath.Dir(c.Args[0]), "zone.db")
		if !strings.Contains(string(data), strconv.Quote(staged)) || strings.Contains(string(data), strconv.Quote(zone)) {
			t.Fatalf("validator sees live zone: %s", data)
		}
		content, err := os.ReadFile(staged)
		if err != nil {
			return nil, err
		}
		if string(content) != "new zone" {
			t.Fatalf("staged content=%s", content)
		}
		return nil, nil
	})
	tx := FileTransaction{Config: TransactionConfig{StageDir: filepath.Join(dir, "stage"), Validate: []Command{{Path: "named-checkconf", Args: []string{"{stageDir}/named.conf"}}}}, Runner: runner}
	original := []byte("zone \"test.internal\" { type primary; file " + strconv.Quote(zone) + "; };\n")
	if err := tx.Apply(context.Background(), map[string][]byte{config: original, zone: []byte("new zone")}); err != nil {
		t.Fatal(err)
	}
	installed, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(installed, original) {
		t.Fatal("installed config retains temporary staging paths")
	}
}

func TestActivationGuardsStructuralChangesBeforeInstallingFiles(t *testing.T) {
	dir := t.TempDir()
	bind := filepath.Join(dir, "bind.conf")
	router := filepath.Join(dir, "dnsdist.conf")
	for _, path := range []string{bind, router} {
		if err := os.WriteFile(path, []byte("approved"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	marker := activationMarkerPath(dir)
	runner := &activationBoundaryRunner{t: t, marker: marker, paths: []string{bind, router}}
	tx := FileTransaction{MarkerPath: marker, Runner: runner, Config: TransactionConfig{
		StageDir: filepath.Join(dir, "stage"), FailClosed: []Command{{Path: "gate"}},
		Reload: []Command{{Path: "reload-bind", WhenChanged: []string{bind}}, {Path: "reload-dnsdist", WhenChanged: []string{router}}},
	}}
	if err := tx.Apply(context.Background(), map[string][]byte{bind: []byte("approved"), router: []byte("candidate")}); err != nil {
		t.Fatal(err)
	}
	if err := tx.commitActivation(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(runner.calls, ",") != "gate,reload-bind,reload-dnsdist" {
		t.Fatalf("owned daemons were not restarted together: %#v", runner.calls)
	}
}

type activationBoundaryRunner struct {
	t      *testing.T
	marker string
	paths  []string
	calls  []string
}

func (r *activationBoundaryRunner) Run(_ context.Context, command Command) ([]byte, error) {
	r.calls = append(r.calls, command.Path)
	_, markerErr := os.Stat(r.marker)
	if command.Path == "gate" {
		if markerErr != nil {
			r.t.Fatal("structural gate ran without durable installation guard")
		}
		for _, path := range r.paths {
			data, err := os.ReadFile(path)
			if err != nil || string(data) != "approved" {
				r.t.Fatal("files changed before DNS was gated")
			}
		}
	} else {
		start, retain, release := daemonPermission(filepath.Dir(r.marker))
		defer release()
		if !start || !retain {
			r.t.Fatal("complete files could not start for validation")
		}
	}
	return nil, nil
}

type blockedInstallRunner struct {
	recordingRunner
	block func() error
}

func (r *blockedInstallRunner) Run(ctx context.Context, command Command) ([]byte, error) {
	if command.Path == "gate" && r.block != nil {
		if err := r.block(); err != nil {
			return nil, err
		}
	}
	return r.recordingRunner.Run(ctx, command)
}

func TestInterruptedInstallationRequiresLocalRecovery(t *testing.T) {
	cfg := regionalTestConfig(t)
	agent, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	installTestPublication(t, agent, time.Now().UTC())
	approved := agent.state.Active.Files
	badParent := filepath.Join(cfg.StateDir, "not-a-directory")
	agent.tx.Runner = &blockedInstallRunner{block: func() error {
		return os.WriteFile(badParent, []byte("blocked"), 0o644)
	}}
	files := map[string][]byte{filepath.Join(badParent, "later.conf"): []byte("candidate")}
	for path := range approved {
		files[path] = []byte("candidate")
	}
	if err := agent.tx.Apply(context.Background(), files); err == nil {
		t.Fatal("interrupted installation succeeded")
	}
	if !activationPending(cfg.StateDir) {
		t.Fatal("interrupted installation lost its durable guard")
	}
	if err := agent.tx.abandonActivation(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.restoreActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if activationPending(cfg.StateDir) || !installedFilesMatch(approved) {
		t.Fatal("approved configuration could not recover without a broker")
	}
}

func TestRecordUpdateReloadsImmutableZoneWithoutStoppingDNS(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "bind.conf")
	zoneToken := "0123456789abcdef01234567"
	before := []byte(`view "tenant-a" { zone "prod.internal" { file "` + dir + `/zone-` + zoneToken + `-0123456789abcdef.db"; }; };`)
	after := []byte(`view "tenant-a" { zone "prod.internal" { file "` + dir + `/zone-` + zoneToken + `-abcdef0123456789.db"; }; };`)
	if err := os.WriteFile(configPath, before, 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}
	tx := FileTransaction{MarkerPath: activationMarkerPath(dir), Runner: runner, Config: TransactionConfig{
		StageDir: filepath.Join(dir, "stage"), FailClosed: []Command{{Path: "gate"}}, Reload: []Command{{Path: "reload-bind"}},
	}}
	if err := tx.Apply(context.Background(), map[string][]byte{configPath: after}); err != nil {
		t.Fatal(err)
	}
	if err := tx.commitActivation(); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 || runner.calls[0].Path != "reload-bind" {
		t.Fatalf("record update stopped healthy DNS: %#v", runner.calls)
	}
	newTenant := bytes.ReplaceAll(after, []byte("tenant-a"), []byte("tenant-b"))
	if !routingConfigurationChanged(configPath, after, newTenant) {
		t.Fatal("tenant routing change did not require a gate")
	}
}
