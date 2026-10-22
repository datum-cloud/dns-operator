// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const loggedPayloadLimit = 16 << 10

type updateFileSummary struct {
	Path   string `json:"path"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
	Lines  int    `json:"lines"`
	Rules  int    `json:"rules"`
}

// logDaemonUpdate records a command that changes a running daemon and returns
// a callback recording its outcome. Payloads are rendered configuration, which
// names the console key file but never contains the key.
func logDaemonUpdate(ctx context.Context, c Command, payload map[string][]byte) func(error) {
	log := logf.FromContext(ctx).WithName("daemon-update").WithValues("daemon", daemonName(c))
	var files []updateFileSummary
	for _, path := range c.WhenChanged {
		data, ok := payload[path]
		if !ok {
			continue
		}
		files = append(files, summarizeUpdate(path, data))
		if verbose := log.V(1); verbose.Enabled() {
			shown, truncated := data, false
			if len(shown) > loggedPayloadLimit {
				shown, truncated = shown[:loggedPayloadLimit], true
			}
			verbose.Info("daemon update payload", "path", path, "truncated", truncated, "payload", string(shown))
		}
	}
	log.Info("sending daemon update", "files", files)
	started := time.Now()
	return func(err error) {
		if err != nil {
			log.Error(err, "daemon update failed", "duration", time.Since(started).String())
			return
		}
		log.Info("daemon update applied", "duration", time.Since(started).String())
	}
}

func daemonName(c Command) string {
	return strings.Join(append([]string{filepath.Base(c.Path)}, c.Args...), " ")
}

func summarizeUpdate(path string, data []byte) updateFileSummary {
	sum := sha256.Sum256(data)
	return updateFileSummary{
		Path:   path,
		Bytes:  len(data),
		SHA256: hex.EncodeToString(sum[:8]),
		Lines:  bytes.Count(data, []byte("\n")),
		Rules:  bytes.Count(data, []byte("appendRule(")) - bytes.Count(data, []byte("function appendRule(")),
	}
}

func oldPayload(old map[string]oldFile) map[string][]byte {
	payload := make(map[string][]byte, len(old))
	for path, file := range old {
		if file.existed {
			payload[path] = file.data
		}
	}
	return payload
}
