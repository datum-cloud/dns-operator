// SPDX-License-Identifier: AGPL-3.0-only

package webhook

import (
	"context"
	"fmt"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	mccontext "sigs.k8s.io/multicluster-runtime/pkg/context"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"

	internaldnsadmission "go.miloapis.com/dns-operator/internal/internaldns/admission"
)

// The private DNS reference checks fail closed: a write that skipped them
// could bind a registration, contribution, association, naming policy, or
// access binding to another tenant's or a replaced object. This marker
// registers them for kind and e2e only; in deployed environments the
// internal-dns control plane registers the same path.
// +kubebuilder:webhook:path=/validate-internal-dns,mutating=false,failurePolicy=fail,sideEffects=None,groups=dns.networking.miloapis.com,resources=dnsrecordsets;dnszoneassociations;dnsregistrations;dnsrecordcontributions;dnsnamingpolicies;dnsresolveraccessbindings,verbs=create;update;delete,versions=v1alpha1,name=vinternaldns.kb.io,admissionReviewVersions=v1
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszones;dnsregistrations;dnsresolvercontexts,verbs=get
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnsregistrations;dnsresolvercontexts,verbs=list
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create

// SetupWebhooks registers every admission handler the dns-operator serves.
func SetupWebhooks(mgr mcmanager.Manager) error {
	if err := SetupDNSRecordSetWebhook(mgr); err != nil {
		return err
	}
	SetupInternalDNSWebhook(mgr)
	return nil
}

// SetupInternalDNSWebhook serves the private DNS reference checks at
// internaldnsadmission.Path. Both server entry points enforce live service-owned
// reservations and delegate authorization to the project API server.
func SetupInternalDNSWebhook(mgr mcmanager.Manager) {
	mgr.GetWebhookServer().Register(internaldnsadmission.Path, &admission.Webhook{
		Handler: &internaldnsadmission.Handler{ClientForRequest: liveProjectClient(mgr)},
	})
}

// liveProjectClient selects the project control plane named by the request's
// cluster context and reads it without a cache, since the checks compare
// lifetimes, UIDs, and generations a cache could serve stale. A request
// without project context reads the local cluster. A named project that cannot
// be found fails the request instead of falling back to another cluster.
func liveProjectClient(mgr mcmanager.Manager) func(context.Context, admission.Request) (client.Client, error) {
	return func(ctx context.Context, _ admission.Request) (client.Client, error) {
		clusterName, ok := mccontext.ClusterFrom(ctx)
		if !ok || clusterName == "" {
			local := mgr.GetLocalManager()
			return liveReads{Client: local.GetClient(), reader: local.GetAPIReader()}, nil
		}
		cl, err := mgr.GetCluster(ctx, clusterName)
		if err != nil && !strings.HasPrefix(clusterName.String(), "/") {
			cl, err = mgr.GetCluster(ctx, "/"+clusterName)
		}
		if err != nil {
			return nil, fmt.Errorf("project control plane %q is unavailable: %w", clusterName, err)
		}
		return liveReads{Client: cl.GetClient(), reader: cl.GetAPIReader()}, nil
	}
}

// liveReads sends reads to the API server instead of the informer cache.
type liveReads struct {
	client.Client
	reader client.Reader
}

func (l liveReads) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return l.reader.Get(ctx, key, obj, opts...)
}

func (l liveReads) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return l.reader.List(ctx, list, opts...)
}
