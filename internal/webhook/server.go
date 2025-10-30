package webhook

import (
	"context"
	"net/http"

	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	mccontext "sigs.k8s.io/multicluster-runtime/pkg/context"

	multiclusterproviders "go.miloapis.com/milo/pkg/multicluster-runtime"
)

type clusterAwareWebhookServer struct {
	webhook.Server
	discoveryMode multiclusterproviders.Provider
}

var _ webhook.Server = &clusterAwareWebhookServer{}

func (s *clusterAwareWebhookServer) Register(path string, hook http.Handler) {
	if h, ok := hook.(*admission.Webhook); ok {
		orig := h.Handler
		h.Handler = admission.HandlerFunc(func(ctx context.Context, req admission.Request) admission.Response {
			c := clusterFromExtra(req.UserInfo.Extra)
			if len(c) > 0 {
				// The cluster names are `<namespace>/<name>` format, but Milo project
				// discovery resources do not have namespaces and return a cluster name
				// with a leading `/`.
				//
				// In the future, this should be improved to allow the multicluster-provider
				// discovery mechanism to handle this somehow.

				c = "/" + c
			}
			ctx = mccontext.WithCluster(ctx, c)
			return orig.Handle(ctx, req)
		})
	}

	s.Server.Register(path, hook)
}

func NewClusterAwareWebhookServer(server webhook.Server, mode multiclusterproviders.Provider) webhook.Server {
	return &clusterAwareWebhookServer{
		Server:        server,
		discoveryMode: mode,
	}
}
