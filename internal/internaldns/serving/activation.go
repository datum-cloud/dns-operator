// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const (
	activationInstalling = "installing"
	activationVerifying  = "verifying"
)

type activationState struct {
	Phase string    `json:"phase"`
	Since time.Time `json:"since"`
}

func readActivation(markerPath string) (activationState, error) {
	var state activationState
	data, err := os.ReadFile(markerPath)
	if err != nil {
		return state, err
	}
	return state, json.Unmarshal(data, &state)
}

func (t *FileTransaction) beginActivation() error {
	if t.MarkerPath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(t.MarkerPath), 0o750); err != nil {
		return err
	}
	if t.activationLock == nil {
		lock, err := os.OpenFile(t.MarkerPath+".lock", os.O_CREATE|os.O_RDWR, 0o640)
		if err != nil {
			return err
		}
		if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
			_ = lock.Close()
			return err
		}
		t.activationLock = lock
	}
	if err := unix.Flock(int(t.activationLock.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	return t.writeActivation(activationState{Phase: activationInstalling})
}

func (t *FileTransaction) writeActivation(state activationState) error {
	if t.MarkerPath == "" {
		return nil
	}
	state.Since = time.Now().UTC()
	if previous, err := readActivation(t.MarkerPath); err == nil && !previous.Since.IsZero() {
		state.Since = previous.Since
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	temporary := t.MarkerPath + ".tmp"
	if err := writeDurable(temporary, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(temporary, t.MarkerPath); err != nil {
		return err
	}
	return syncDirs([]string{t.MarkerPath})
}

func (t *FileTransaction) filesInstalled() error {
	if err := t.writeActivation(activationState{Phase: activationVerifying}); err != nil {
		return err
	}
	if t.activationLock != nil {
		return unix.Flock(int(t.activationLock.Fd()), unix.LOCK_SH)
	}
	return nil
}

func (t *FileTransaction) commitActivation() error {
	if t.MarkerPath != "" {
		if err := os.Remove(t.MarkerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := syncDirs([]string{t.MarkerPath}); err != nil {
			return err
		}
	}
	return t.abandonActivation()
}

// The kernel releases activation ownership when the agent process exits.
func (t *FileTransaction) abandonActivation() error {
	if t.activationLock == nil {
		return nil
	}
	err := t.activationLock.Close()
	t.activationLock = nil
	return err
}

// daemonPermission holds the shared activation lock through daemon startup so
// installation cannot race with a process loading its configuration files.
func daemonPermission(stateDir string) (start, retain bool, release func()) {
	marker := activationMarkerPath(stateDir)
	lock, err := os.Open(marker + ".lock")
	if err != nil {
		return false, false, func() {}
	}
	release = func() { _ = lock.Close() }
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			// Running processes retain their in-memory configuration while the
			// installer writes files. New processes must wait.
			return false, true, release
		}
		return false, false, release
	}
	upgradeErr := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	_, readErr := os.ReadFile(marker)
	if errors.Is(readErr, os.ErrNotExist) {
		return true, true, release
	}
	if upgradeErr == nil {
		// A pending marker without an installer is an abandoned activation.
		return false, false, release
	}
	if !errors.Is(upgradeErr, unix.EWOULDBLOCK) {
		return false, false, release
	}
	// A failed upgrade may release the shared lock. Reacquire it before
	// reading the phase or allowing a daemon to load its configuration.
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
		return false, true, release
	}
	data, readErr := os.ReadFile(marker)
	if errors.Is(readErr, os.ErrNotExist) {
		return true, true, release
	}
	var state activationState
	if readErr != nil || json.Unmarshal(data, &state) != nil || state.Phase != activationVerifying {
		return false, false, release
	}
	return true, true, release
}
