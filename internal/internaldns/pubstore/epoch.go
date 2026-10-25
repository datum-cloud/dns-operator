// SPDX-License-Identifier: AGPL-3.0-only

package pubstore

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	dnsv1 "go.miloapis.com/dns-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// counterEpochCeiling separates writer epochs that count takeovers from one
// (every epoch written before epochs were raised to wall-clock seconds) from
// wall-clock epochs.
const counterEpochCeiling = 1_000_000_000

// RaiseWriterEpoch lifts a counter epoch to the current Unix time, so a writer
// whose ownership record was lost or moved to another store still outranks
// every fence serving agents hold. A zone lease is raised on any write: its
// writer reads the epoch back from the write. A shard lease is raised only when
// it is created, because the planner keeps using its own copy of the epoch
// after an update.
func RaiseWriterEpoch(obj client.Object, creating bool, now time.Time) {
	floor := now.Unix()
	switch o := obj.(type) {
	case *dnsv1.DNSPublicationOwnership:
		if o.Spec.WriterEpoch < counterEpochCeiling {
			o.Spec.WriterEpoch = max(o.Spec.WriterEpoch, floor)
		}
	case *corev1.ConfigMap:
		if !creating || !strings.HasPrefix(o.Name, shardOwnerPrefix) || o.Data["state"] == "" {
			return
		}
		var state map[string]any
		if err := json.Unmarshal([]byte(o.Data["state"]), &state); err != nil {
			return
		}
		if epoch, _ := state["epoch"].(float64); epoch < counterEpochCeiling {
			state["epoch"] = max(int64(epoch), floor)
			if b, err := json.Marshal(state); err == nil {
				o.Data["state"] = string(b)
			}
		}
	}
}

// EpochFloorClient raises writer epochs on the API server path, so switching
// publication stores in either direction never leaves agents fenced against
// the new writer.
type EpochFloorClient struct {
	client.Client
	Now func() time.Time
}

func (c *EpochFloorClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	RaiseWriterEpoch(obj, true, c.Now())
	return c.Client.Create(ctx, obj, opts...)
}

func (c *EpochFloorClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	RaiseWriterEpoch(obj, false, c.Now())
	return c.Client.Update(ctx, obj, opts...)
}

func (c *EpochFloorClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	RaiseWriterEpoch(obj, false, c.Now())
	return c.Client.Patch(ctx, obj, patch, opts...)
}
