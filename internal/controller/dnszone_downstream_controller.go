/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	pdnsclient "go.miloapis.com/dns-operator/internal/pdns"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// DNSZoneReconciler reconciles a DNSZone object
type DNSZoneReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszones,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszones/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszones/finalizers,verbs=update
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszoneclasses,verbs=get;list;watch

// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.1/pkg/reconcile
func (r *DNSZoneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)
	logger.Info("dnszone reconcile start", "namespace", req.Namespace, "name", req.Name)

	var zone dnsv1alpha1.DNSZone
	if err := r.Get(ctx, req.NamespacedName, &zone); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// If class is set and equals "powerdns", ensure in PDNS (status handled by replicator)
	if zone.Spec.DNSZoneClassName != "" {
		var zc dnsv1alpha1.DNSZoneClass
		if err := r.Get(ctx, client.ObjectKey{Name: zone.Spec.DNSZoneClassName}, &zc); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		if zc.Spec.ControllerName == ControllerNamePowerDNS {
			cli, err := pdnsclient.NewFromEnv()
			if err != nil {
				logger.Error(err, "pdns client init")
				return ctrl.Result{}, fmt.Errorf("pdns client: %w", err)
			}
			// Ensure zone exists or create (no status updates here)
			if _, err := cli.GetZone(ctx, zone.Spec.DomainName); err != nil {
				// nameservers from class policy (Static)
				var nss []string
				if zc.Spec.NameServerPolicy.Mode == dnsv1alpha1.NameServerPolicyModeStatic && zc.Spec.NameServerPolicy.Static != nil {
					nss = append(nss, zc.Spec.NameServerPolicy.Static.Servers...)
				}
				if err := cli.CreateZone(ctx, zone.Spec.DomainName, nss); err != nil {
					logger.Error(err, "create pdns zone")
					return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
				}
			}
		}
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *DNSZoneReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dnsv1alpha1.DNSZone{}).
		Named("dnszone").
		Complete(r)
}
