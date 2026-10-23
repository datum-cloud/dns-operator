package serving

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Command struct {
	Daemon      Daemon   `json:"daemon,omitempty"`
	Path        string   `json:"path"`
	Args        []string `json:"args,omitempty"`
	WhenChanged []string `json:"whenChanged,omitempty"`
}

type Daemon string

const (
	DaemonCoreDNS Daemon = "coredns"
	DaemonDNSDist Daemon = "dnsdist"
)

func (c TransactionConfig) validateDaemons() error {
	for _, commands := range [][]Command{c.Validate, c.Reload, c.FailClosed} {
		for _, command := range commands {
			switch command.Daemon {
			case "", DaemonCoreDNS, DaemonDNSDist:
			default:
				return fmt.Errorf("command %s names unknown daemon %q", command.Path, command.Daemon)
			}
		}
	}
	return nil
}

type Runner interface {
	Run(context.Context, Command) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, c Command) ([]byte, error) {
	if c.Path == "" {
		return nil, errors.New("command path is required")
	}
	return exec.CommandContext(ctx, c.Path, c.Args...).CombinedOutput()
}

type TransactionConfig struct {
	StageDir        string    `json:"stageDir"`
	CommandStageDir string    `json:"commandStageDir,omitempty"`
	Validate        []Command `json:"validate"`
	Reload          []Command `json:"reload"`
	FailClosed      []Command `json:"failClosed,omitempty"`
}

type FileTransaction struct {
	activationLock *os.File
	MarkerPath     string
	Config         TransactionConfig
	Runner         Runner
	restarts       uint64
}

type preservedTransactionError struct{ error }

func (e preservedTransactionError) Unwrap() error { return e.error }

type uncertainTransactionError struct{ error }

func (e uncertainTransactionError) Unwrap() error { return e.error }

type oldFile struct {
	data    []byte
	mode    os.FileMode
	existed bool
}

func (t *FileTransaction) Apply(ctx context.Context, files map[string][]byte) error {
	return t.apply(ctx, files, false)
}

// ApplyForce validates and reloads every configured daemon even when the
// installed bytes already match. It is used after fail-closed startup because
// file equality cannot prove that a stopped or firewalled process is active.
func (t *FileTransaction) ApplyForce(ctx context.Context, files map[string][]byte) error {
	return t.apply(ctx, files, true)
}

func (t *FileTransaction) apply(ctx context.Context, files map[string][]byte, force bool) (resultErr error) {
	mutating := false
	defer func() {
		if resultErr == nil {
			return
		}
		var preserved preservedTransactionError
		if errors.As(resultErr, &preserved) {
			return
		}
		if mutating {
			resultErr = uncertainTransactionError{resultErr}
		} else {
			resultErr = preservedTransactionError{resultErr}
		}
	}()
	if t.Runner == nil {
		t.Runner = ExecRunner{}
	}
	if !filepath.IsAbs(t.Config.StageDir) || len(files) == 0 {
		return errors.New("absolute stage directory and rendered files are required")
	}
	if err := os.MkdirAll(t.Config.StageDir, 0o750); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(t.Config.StageDir, ".internal-dns-stage-")
	if err != nil {
		return err
	}
	// Validators commonly drop privileges inside their container.  The staged
	// directory and generated routing configuration contain no credentials, so
	// make them traversable/readable by those service accounts.
	if err := os.Chmod(stage, 0o755); err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	paths := make([]string, 0, len(files))
	seenBase := map[string]bool{}
	for path := range files {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("rendered path %q is not absolute", path)
		}
		base := filepath.Base(path)
		if seenBase[base] {
			return fmt.Errorf("duplicate rendered basename %q", base)
		}
		seenBase[base] = true
		paths = append(paths, path)
	}
	sort.Strings(paths)
	old := map[string]oldFile{}
	changed := map[string]bool{}
	for _, path := range paths {
		b, e := os.ReadFile(path)
		if e == nil {
			info, _ := os.Stat(path)
			old[path] = oldFile{data: b, mode: info.Mode().Perm(), existed: true}
			changed[path] = string(b) != string(files[path])
		} else if !errors.Is(e, os.ErrNotExist) {
			return e
		} else {
			changed[path] = true
		}
	}
	stagedPaths := make([]string, 0, len(paths))
	for _, path := range paths {
		stagedPath := filepath.Join(stage, filepath.Base(path))
		stagedData := files[path]

		if err := writeDurable(stagedPath, stagedData, 0o644); err != nil {
			return err
		}
		stagedPaths = append(stagedPaths, stagedPath)
	}
	// Persist directory entries before a validator in another container reads
	// this shared bind mount. File fsync alone does not make new names durable.
	if err := syncDirs(stagedPaths); err != nil {
		return err
	}
	commandStage := stage
	if t.Config.CommandStageDir != "" {
		commandStage = filepath.Join(t.Config.CommandStageDir, filepath.Base(stage))
	}
	commandChanges := changed
	if force {
		commandChanges = nil
	}
	if err := t.commandsForChanges(ctx, t.Config.Validate, map[string]string{"{stageDir}": commandStage}, commandChanges, nil, false); err != nil {
		return preservedTransactionError{fmt.Errorf("validate staged serving configuration: %w", err)}
	}
	if t.MarkerPath != "" {
		structural := force
		for _, path := range paths {
			structural = structural || changed[path] && routingConfigurationChanged(path, old[path].data, files[path])
		}
		if err := t.beginActivation(); err != nil {
			return err
		}
		if structural {
			// Restart every owned daemon after changing routing or view configuration.
			commandChanges = nil
			t.restarts++
			if err := t.FailClosed(ctx); err != nil {
				return uncertainTransactionError{err}
			}
		}
	}
	installed := make([]string, 0, len(paths))
	mutating = true
	for _, path := range paths {
		if !changed[path] {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			_ = restoreFiles(installed, old)
			return err
		}
		// Installed configuration refers to live zone paths; staging rewrites exist
		// only for validation. Prepare original bytes before atomic rename.
		if err := writeDurable(filepath.Join(stage, filepath.Base(path)), files[path], 0o644); err != nil {
			return errors.Join(err, restoreFiles(installed, old))
		}
		if err := os.Rename(filepath.Join(stage, filepath.Base(path)), path); err != nil {
			_ = restoreFiles(installed, old)
			return err
		}
		installed = append(installed, path)
	}
	if err := syncDirs(installed); err != nil {
		rollbackErr := restoreFiles(installed, old)
		return errors.Join(err, rollbackErr)
	}
	if err := t.filesInstalled(); err != nil {
		return err
	}
	if err := t.commandsForChanges(ctx, t.Config.Reload, nil, commandChanges, files, true); err != nil {
		if t.MarkerPath != "" {
			if guardErr := t.beginActivation(); guardErr != nil {
				return errors.Join(err, guardErr)
			}
			commandChanges = nil
			t.restarts++
			if guardErr := t.FailClosed(ctx); guardErr != nil {
				return errors.Join(err, guardErr)
			}
		}
		rollbackErr := errors.Join(restoreFiles(paths, old), syncDirs(paths))
		if rollbackErr != nil {
			return errors.Join(err, rollbackErr)
		}
		if guardErr := t.filesInstalled(); guardErr != nil {
			return errors.Join(err, guardErr)
		}
		reloadErr := t.commandsForChanges(ctx, t.Config.Reload, nil, commandChanges, oldPayload(old), true)
		if rollbackErr == nil && reloadErr == nil {
			if guardErr := t.commitActivation(); guardErr != nil {
				return errors.Join(err, guardErr)
			}
			return preservedTransactionError{fmt.Errorf("reload serving configuration: %w", err)}
		}
		return errors.Join(fmt.Errorf("reload serving configuration: %w", err), rollbackErr, reloadErr)
	}
	return nil
}

func (t FileTransaction) FailClosed(ctx context.Context) error {
	if t.Runner == nil {
		t.Runner = ExecRunner{}
	}
	return t.commandsForChanges(ctx, t.Config.FailClosed, nil, nil, nil, true)
}

func (t FileTransaction) commandsForChanges(ctx context.Context, commands []Command, replacements map[string]string, changed map[string]bool, payload map[string][]byte, daemonUpdate bool) error {
	for _, c := range commands {
		// Command is copied by value, but its Args slice would otherwise retain
		// replacements in the configured transaction. Every Apply uses a fresh
		// staging directory, so never mutate the shared backing array.
		c.Args = append([]string(nil), c.Args...)
		if changed != nil && len(c.WhenChanged) > 0 {
			run := false
			for _, path := range c.WhenChanged {
				if changed[path] {
					run = true
					break
				}
			}
			if !run {
				continue
			}
		}
		c.Path = replace(c.Path, replacements)
		for i := range c.Args {
			c.Args[i] = replace(c.Args[i], replacements)
		}
		finished := func(error) {}
		if daemonUpdate {
			finished = logDaemonUpdate(ctx, c, payload)
		}
		out, err := t.Runner.Run(ctx, c)
		finished(err)
		if err != nil {
			return fmt.Errorf("%s %s: %w: %s", c.Path, strings.Join(c.Args, " "), err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}
func replace(s string, r map[string]string) string {
	for old, newValue := range r {
		s = strings.ReplaceAll(s, old, newValue)
	}
	return s
}

func writeDurable(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func syncDirs(paths []string) error {
	seen := map[string]bool{}
	for _, path := range paths {
		dir := filepath.Dir(path)
		if seen[dir] {
			continue
		}
		seen[dir] = true
		f, err := os.Open(dir)
		if err != nil {
			return err
		}
		err = f.Sync()
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}
func restoreFiles(paths []string, old map[string]oldFile) error {
	var errs []error
	for _, path := range paths {
		value := old[path]
		if !value.existed {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
			continue
		}
		temp := path + fmt.Sprintf(".rollback-%d", time.Now().UnixNano())
		if err := writeDurable(temp, value.data, value.mode); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := os.Rename(temp, path); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func routingConfigurationChanged(path string, before, after []byte) bool {
	return strings.HasSuffix(path, ".conf") && !strings.HasSuffix(path, "-dnsdist.conf") && !bytes.Equal(before, after)
}
