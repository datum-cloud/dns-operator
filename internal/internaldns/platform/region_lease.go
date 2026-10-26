// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// RegionLease elects one control-plane process per region to run the region's
// outbox and acknowledgement refresh. It only takes and renews a lease, so a
// failure elsewhere in the control plane cannot keep it from leading.
type RegionLease struct {
	Client        client.Client
	Namespace     string
	Region        string
	Identity      string
	LeaseDuration time.Duration
	Now           func() time.Time
	// Stopping, when set and true, stops the lease being taken or renewed so
	// the process can release it before it exits.
	Stopping func() bool

	leadUntil atomic.Int64
}

type regionLeaseState struct {
	Holder     string    `json:"holder"`
	LeaseUntil time.Time `json:"leaseUntil"`
}

func (l *RegionLease) key() client.ObjectKey {
	return client.ObjectKey{Namespace: l.Namespace, Name: "dns-region-leader-" + model.OpaqueToken(l.Region)}
}

func (l *RegionLease) now() time.Time {
	if l.Now != nil {
		return l.Now().UTC()
	}
	return time.Now().UTC()
}

func (l *RegionLease) duration() time.Duration {
	if l.LeaseDuration <= 0 {
		return 30 * time.Second
	}
	return l.LeaseDuration
}

// Leads reports whether this process holds the region lease.
func (l *RegionLease) Leads(now time.Time) bool {
	return now.UnixNano() < l.leadUntil.Load()
}

// Step takes the lease when it is free or expired, and renews it once a third
// of it has passed.
func (l *RegionLease) Step(ctx context.Context) error {
	if l.Stopping != nil && l.Stopping() {
		l.leadUntil.Store(0)
		return nil
	}
	now, lease := l.now(), l.duration()
	var cm corev1.ConfigMap
	err := l.Client.Get(ctx, l.key(), &cm)
	if apierrors.IsNotFound(err) {
		b, _ := json.Marshal(regionLeaseState{Holder: l.Identity, LeaseUntil: now.Add(lease)})
		cm = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: l.key().Name, Namespace: l.Namespace, Labels: map[string]string{"internal-dns.miloapis.com/region": model.SafeToken(l.Region)}}, Data: map[string]string{"state": string(b)}}
		if err := l.Client.Create(ctx, &cm); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return nil
			}
			return err
		}
		l.leadUntil.Store(now.Add(lease).UnixNano())
		return nil
	}
	if err != nil {
		return err
	}
	var state regionLeaseState
	if err := json.Unmarshal([]byte(cm.Data["state"]), &state); err != nil {
		return fmt.Errorf("invalid region lease: %w", err)
	}
	if state.Holder != l.Identity && state.LeaseUntil.After(now) {
		l.leadUntil.Store(0)
		return nil
	}
	if state.Holder == l.Identity && state.LeaseUntil.Sub(now) > lease*2/3 {
		l.leadUntil.Store(state.LeaseUntil.UnixNano())
		return nil
	}
	state = regionLeaseState{Holder: l.Identity, LeaseUntil: now.Add(lease)}
	b, _ := json.Marshal(state)
	cm.Data["state"] = string(b)
	if err := l.Client.Update(ctx, &cm); err != nil {
		if apierrors.IsConflict(err) {
			l.leadUntil.Store(0)
			return nil
		}
		return err
	}
	l.leadUntil.Store(state.LeaseUntil.UnixNano())
	return nil
}

// Release ends the lease if this process holds it, so another process leads
// the region at once. It reports whether it released a lease.
func (l *RegionLease) Release(ctx context.Context) (bool, error) {
	l.leadUntil.Store(0)
	now := l.now()
	var cm corev1.ConfigMap
	if err := l.Client.Get(ctx, l.key(), &cm); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	var state regionLeaseState
	if err := json.Unmarshal([]byte(cm.Data["state"]), &state); err != nil {
		return false, fmt.Errorf("invalid region lease: %w", err)
	}
	if state.Holder != l.Identity || !state.LeaseUntil.After(now) {
		return false, nil
	}
	state.LeaseUntil = now
	b, _ := json.Marshal(state)
	cm.Data["state"] = string(b)
	if err := l.Client.Update(ctx, &cm); err != nil {
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
