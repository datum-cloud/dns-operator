// SPDX-License-Identifier: AGPL-3.0-only

package webhook

import (
	"context"
	"net/http"

	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	mccontext "sigs.k8s.io/multicluster-runtime/pkg/context"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
)

// clusterAwareWebhookServer wraps a webhook.Server to inject the project
// control plane cluster name from admission UserInfo.Extra into the context.
// Copied locally because milo v0.7.4 does not expose pkg/webhook.
type clusterAwareWebhookServer struct {
	webhook.Server
}

var _ webhook.Server = &clusterAwareWebhookServer{}

// NewClusterAwareWebhookServer wraps server so registered admission handlers
// see mccontext.ClusterFrom(ctx) when the request targets a Project.
func NewClusterAwareWebhookServer(server webhook.Server) webhook.Server {
	return &clusterAwareWebhookServer{Server: server}
}

// Register wraps admission webhook handlers to inject cluster context.
func (s *clusterAwareWebhookServer) Register(path string, hook http.Handler) {
	if h, ok := hook.(*admission.Webhook); ok {
		orig := h.Handler
		h.Handler = admission.HandlerFunc(func(ctx context.Context, req admission.Request) admission.Response {
			if clusterName := clusterNameFromExtra(req.UserInfo.Extra); clusterName != "" {
				ctx = mccontext.WithCluster(ctx, multicluster.ClusterName(clusterName))
			}
			return orig.Handle(ctx, req)
		})
	}
	s.Server.Register(path, hook)
}
