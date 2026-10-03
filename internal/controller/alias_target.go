// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

const (
	aliasTargetCheckInterval = 5 * time.Minute
	aliasTargetLookupTimeout = 5 * time.Second
)

type TargetResolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

var aliasTargetUnresolved = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "dns_operator_alias_target_unresolved",
	Help: "1 when a DNSRecordSet's ALIAS target resolves to no A or AAAA address, so the name answers SERVFAIL.",
}, []string{"namespace", "name"})

func init() {
	ctrlmetrics.Registry.MustRegister(aliasTargetUnresolved)
}

type aliasTargetChecks struct {
	mu   sync.Mutex
	last map[types.NamespacedName]aliasTargetCheck
}

type aliasTargetCheck struct {
	generation int64
	at         time.Time
}

func (c *aliasTargetChecks) due(key types.NamespacedName, generation int64, now time.Time) (time.Duration, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prev, ok := c.last[key]
	if !ok || prev.generation != generation {
		return 0, true
	}
	if wait := prev.at.Add(aliasTargetCheckInterval).Sub(now); wait > 0 {
		return wait, false
	}
	return 0, true
}

func (c *aliasTargetChecks) record(key types.NamespacedName, generation int64, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last == nil {
		c.last = map[types.NamespacedName]aliasTargetCheck{}
	}
	c.last[key] = aliasTargetCheck{generation: generation, at: now}
}

func (c *aliasTargetChecks) forget(key types.NamespacedName) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.last, key)
	aliasTargetUnresolved.DeleteLabelValues(key.Namespace, key.Name)
}

// PowerDNS expands ALIAS at query time, so a target that stops resolving turns
// the name into SERVFAIL while the write itself stays programmed.
func (r *DNSRecordSetReconciler) checkALIASTargets(ctx context.Context, rs *dnsv1alpha1.DNSRecordSet) (ctrl.Result, error) {
	key := client.ObjectKeyFromObject(rs)
	if r.TargetResolver == nil {
		return ctrl.Result{}, nil
	}
	if rs.Spec.RecordType != dnsv1alpha1.RRTypeALIAS {
		r.aliasChecks.forget(key)
		return ctrl.Result{}, nil
	}
	now := time.Now()
	if wait, due := r.aliasChecks.due(key, rs.Generation, now); !due {
		return ctrl.Result{RequeueAfter: wait}, nil
	}

	cond := aliasTargetCondition(ctx, r.TargetResolver, rs)
	r.aliasChecks.record(key, rs.Generation, now)
	if cond.Status == metav1.ConditionFalse {
		aliasTargetUnresolved.WithLabelValues(rs.Namespace, rs.Name).Set(1)
	} else {
		aliasTargetUnresolved.DeleteLabelValues(rs.Namespace, rs.Name)
	}

	base := rs.DeepCopy()
	if apimeta.SetStatusCondition(&rs.Status.Conditions, cond) {
		if err := r.Status().Patch(ctx, rs, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: aliasTargetCheckInterval}, nil
}

func aliasTargetCondition(ctx context.Context, resolver TargetResolver, rs *dnsv1alpha1.DNSRecordSet) metav1.Condition {
	var unresolved, failed []string
	seen := map[string]struct{}{}
	for _, rec := range rs.Spec.Records {
		if rec.ALIAS == nil {
			continue
		}
		target := strings.TrimSuffix(strings.TrimSpace(rec.ALIAS.Content), ".")
		if target == "" {
			continue
		}
		target += "."
		if _, ok := seen[target]; ok {
			continue
		}
		seen[target] = struct{}{}

		lookupCtx, cancel := context.WithTimeout(ctx, aliasTargetLookupTimeout)
		addrs, err := resolver.LookupIPAddr(lookupCtx, target)
		cancel()
		var dnsErr *net.DNSError
		switch {
		case err == nil && len(addrs) > 0:
		case err == nil, errors.As(err, &dnsErr) && dnsErr.IsNotFound:
			unresolved = append(unresolved, target)
		default:
			failed = append(failed, target)
		}
	}
	sort.Strings(unresolved)
	sort.Strings(failed)

	cond := metav1.Condition{
		Type:               CondTargetResolved,
		ObservedGeneration: rs.Generation,
		LastTransitionTime: metav1.Now(),
	}
	switch {
	case len(unresolved) > 0:
		cond.Status = metav1.ConditionFalse
		cond.Reason = ReasonTargetUnresolved
		cond.Message = fmt.Sprintf(
			"ALIAS target %s does not resolve to an A or AAAA address, so the name answers SERVFAIL",
			strings.Join(unresolved, ", "))
	case len(failed) > 0:
		cond.Status = metav1.ConditionUnknown
		cond.Reason = ReasonTargetLookupFailed
		cond.Message = fmt.Sprintf("could not look up ALIAS target %s; retrying", strings.Join(failed, ", "))
	default:
		cond.Status = metav1.ConditionTrue
		cond.Reason = ReasonTargetResolved
		cond.Message = "every ALIAS target resolves"
	}
	return cond
}

// NewTargetResolver returns the system resolver, or one that sends every query
// to address (host:port) when it is set.
func NewTargetResolver(address string) *net.Resolver {
	if address == "" {
		return net.DefaultResolver
	}
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, address)
		},
	}
}
