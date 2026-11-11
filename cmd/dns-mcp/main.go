// SPDX-License-Identifier: AGPL-3.0-only

// Command dns-mcp serves this operator's read-only diagnostic tools over MCP,
// alongside the knowledge and skills an assistant reads before calling them:
//
//	POST /mcp                   Streamable HTTP MCP, stateless
//	GET  /llms-full.txt         Knowledge: the DNS resource model
//	GET  /runbooks/<name>.md    Skills: triage procedures
//	GET  /healthz               liveness
//
// One process, because the capability document names all of those URLs. Only
// /mcp takes a credential; see docs.go for why the documents do not.
//
// The server holds no credential of its own for the project control plane: it
// reads through a client built from the caller's own bearer token. So a tool
// call can never see more than the person who asked, the platform's RBAC stays
// the single enforcement point, and there is no impersonation privilege here
// to escalate with.
//
// The project a request reads is taken from a header, never from a tool
// argument: arguments are chosen by the model, and a model that could name its
// own project would be one prompt injection away from another tenant's zones.
// The header is set by the already-authenticated caller.
//
// There is deliberately no mutating tool published here. See
// internal/agent/tools.go for why.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/agent"
)

const (
	// projectHeader names the project whose control plane a request reads.
	// Set by the caller, not by the model.
	projectHeader = "X-Datum-Project"

	// serverName and serverVersion identify this server to MCP clients.
	serverName    = "datum-dns-mcp"
	serverVersion = "0.1.0"

	// readHeaderTimeout bounds how long a slow client may hold a connection
	// before sending headers.
	readHeaderTimeout = 10 * time.Second

	// shutdownTimeout bounds how long in-flight requests may run after the
	// process is told to stop.
	shutdownTimeout = 15 * time.Second

	// misconfiguredClientNote closes every error a request can fail with
	// before a read is attempted. All of them are the calling client's
	// configuration.
	misconfiguredClientNote = "The person who asked did nothing wrong and re-authenticating will not " +
		"help: this is a configuration problem for whoever operates that client"

	// resourceNamespace is where this operator's objects live inside a
	// project's control plane: the project routes to the control plane, and
	// within it everything is in "default". Mirrors
	// internal/cmd/dns/util.ResourceNamespace, not imported because that
	// package pulls in the datumctl plugin runtime.
	resourceNamespace = "default"

	// resourceManagerGroup and resourceManagerVersion address a project's
	// virtual control plane. Mirrors internal/cmd/dns/util.ProjectControlPlaneURL.
	resourceManagerGroup   = "resourcemanager.miloapis.com"
	resourceManagerVersion = "v1alpha1"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(dnsv1alpha1.AddToScheme(scheme))
}

func main() {
	var addr, tlsCertFile, tlsKeyFile string

	flag.StringVar(&addr, "addr", envOr("DNS_MCP_ADDR", ":8080"),
		"address to serve MCP on")
	flag.StringVar(&tlsCertFile, "tls-cert-file", os.Getenv("DNS_MCP_TLS_CERT_FILE"),
		"certificate to serve HTTPS with; requires --tls-key-file. Plain HTTP when both are unset")
	flag.StringVar(&tlsKeyFile, "tls-key-file", os.Getenv("DNS_MCP_TLS_KEY_FILE"),
		"private key to serve HTTPS with; requires --tls-cert-file")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// GetConfig resolves the --kubeconfig flag that controller-runtime
	// registers, then KUBECONFIG, then in-cluster config, then
	// ~/.kube/config. Only the endpoint and CA are used; see clientConfig.
	baseConfig, err := ctrl.GetConfig()
	if err != nil {
		setupLog.Error(err, "unable to load control plane configuration")
		os.Exit(1)
	}

	if err := checkControlPlaneEndpoint(baseConfig); err != nil {
		setupLog.Error(err, "refusing to start")
		os.Exit(1)
	}

	if err := run(ctrl.SetupSignalHandler(), addr, tlsCertFile, tlsKeyFile, baseConfig); err != nil {
		setupLog.Error(err, "server failed")
		os.Exit(1)
	}
}

// checkControlPlaneEndpoint refuses to start when the configuration resolved
// to the API server of the cluster this process runs in.
//
// In a pod with no kubeconfig, GetConfig falls back to in-cluster config, and
// clientConfig then hangs a project control-plane path off the local API
// server. Every tool call fails with a 401 that reads like the caller's token
// is bad when the deployment is what is wrong; failing at boot puts the error
// where the mistake is.
func checkControlPlaneEndpoint(cfg *rest.Config) error {
	local := localClusterEndpoint()
	if local == "" || !sameEndpoint(cfg.Host, local) {
		return nil
	}
	return fmt.Errorf(
		"control plane endpoint %s is this cluster's own API server: dns-mcp reads Datum project "+
			"control planes, not the cluster it runs in, so the in-cluster fallback is never correct. "+
			"Give the deployment an explicit control plane: mount a kubeconfig naming the control "+
			"plane's address and CA, and point KUBECONFIG at it (or pass --kubeconfig)",
		cfg.Host)
}

// localClusterEndpoint returns the API server address in-cluster config
// resolves to, or "" outside a pod. It mirrors rest.InClusterConfig's own
// derivation rather than calling it, so the check still holds when the
// projected ServiceAccount token InClusterConfig also requires is not
// mounted.
func localClusterEndpoint() string {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return ""
	}
	return "https://" + net.JoinHostPort(host, port)
}

// sameEndpoint compares two API server addresses, ignoring a trailing slash.
func sameEndpoint(a, b string) bool {
	return strings.TrimSuffix(a, "/") == strings.TrimSuffix(b, "/")
}

func run(ctx context.Context, addr, tlsCertFile, tlsKeyFile string, baseConfig *rest.Config) error {
	if err := checkTLSFiles(tlsCertFile, tlsKeyFile); err != nil {
		return err
	}

	handler, err := newHandler(baseConfig)
	if err != nil {
		return err
	}

	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}
	return serve(ctx, listener, handler, tlsCertFile, tlsKeyFile)
}

// checkTLSFiles rejects half a TLS configuration. Falling back to plain HTTP
// when one file is missing would silently drop the transport security the
// deployment asked for.
func checkTLSFiles(certFile, keyFile string) error {
	if (certFile == "") == (keyFile == "") {
		return nil
	}
	return errors.New("--tls-cert-file and --tls-key-file must be set together: " +
		"set both to serve HTTPS, or neither to serve plain HTTP")
}

func newHandler(baseConfig *rest.Config) (http.Handler, error) {
	mcpHandler := mcp.NewStreamableHTTPHandler(
		func(r *http.Request) *mcp.Server {
			// A server per request, bound to that caller's identity and
			// project. Nothing is shared between callers.
			s := mcp.NewServer(&mcp.Implementation{
				Name:    serverName,
				Version: serverVersion,
			}, nil)
			agent.RegisterTools(s, depsFromRequest(r, baseConfig))
			return s
		},
		// Stateless: no session state is needed for read-only tools, and it
		// keeps the server robust against client crashes.
		&mcp.StreamableHTTPOptions{Stateless: true},
	)

	// Served from this same process, so one deployment satisfies every URL
	// the capability document points the assistant at.
	docs, err := newKnowledgeHandler()
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpHandler)
	mux.Handle(knowledgePath, docs)
	mux.Handle(runbookPrefix, docs)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	})

	setupLog.Info("serving", "mcp", "/mcp", "docs", docs.paths())
	return mux, nil
}

// serve answers on listener until ctx is done, over HTTPS when a certificate
// is given and plain HTTP otherwise. The certificate is re-read when its files
// change, so a rotation needs no restart.
func serve(ctx context.Context, listener net.Listener, handler http.Handler, tlsCertFile, tlsKeyFile string) error {
	if err := checkTLSFiles(tlsCertFile, tlsKeyFile); err != nil {
		_ = listener.Close()
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	scheme := "http"
	var watcherDone chan error
	if tlsCertFile != "" {
		watcher, err := certwatcher.New(tlsCertFile, tlsKeyFile)
		if err != nil {
			_ = listener.Close()
			return fmt.Errorf("loading TLS certificate: %w", err)
		}
		server.TLSConfig = &tls.Config{
			MinVersion:     tls.VersionTLS12,
			GetCertificate: watcher.GetCertificate,
		}
		scheme = "https"
		watcherDone = make(chan error, 1)
		go func() { watcherDone <- watcher.Start(ctx) }()
	}

	serveDone := make(chan error, 1)
	go func() {
		if server.TLSConfig != nil {
			serveDone <- server.ServeTLS(listener, "", "")
			return
		}
		serveDone <- server.Serve(listener)
	}()

	setupLog.Info("listening", "scheme", scheme, "addr", listener.Addr().String())

	var failure error
	select {
	case err := <-serveDone:
		failure = fmt.Errorf("serving %s on %s: %w", scheme, listener.Addr(), err)
	case err := <-watcherDone:
		watcherDone = nil
		if err != nil {
			failure = fmt.Errorf("watching TLS certificate: %w", err)
		}
	case <-ctx.Done():
	}

	cancel()
	shutdownCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer stop()
	if err := server.Shutdown(shutdownCtx); err != nil && failure == nil {
		failure = fmt.Errorf("shutting down: %w", err)
	}
	if watcherDone != nil {
		if err := <-watcherDone; err != nil && failure == nil {
			failure = fmt.Errorf("watching TLS certificate: %w", err)
		}
	}
	return failure
}

// depsFromRequest returns a DepsFor that resolves the caller's identity and
// project from r. Resolution is deferred to call time so a request carrying
// no credentials fails as a tool error the model can report, rather than as
// a nil client panic deep in a handler.
func depsFromRequest(r *http.Request, baseConfig *rest.Config) agent.DepsFor {
	token := bearerToken(r)
	project := strings.TrimSpace(r.Header.Get(projectHeader))

	return func(context.Context) (agent.ToolDeps, error) {
		if token == "" {
			return agent.ToolDeps{}, fmt.Errorf(
				"no credentials on this request: the client that called this tool did not forward "+
					"the user's identity. %s", misconfiguredClientNote)
		}
		if project == "" {
			return agent.ToolDeps{}, fmt.Errorf(
				"no project on this request: the client that called this tool did not set the %s "+
					"header. %s", projectHeader, misconfiguredClientNote)
		}

		c, err := clientForToken(baseConfig, token, project)
		if err != nil {
			return agent.ToolDeps{}, err
		}
		return agent.ToolDeps{Reader: agent.NewClientReader(c), Namespace: resourceNamespace}, nil
	}
}

// projectControlPlanePath returns the API path for a project's control
// plane. A project is addressed by rewriting the host path, not by a
// namespace named after it — the same rewrite
// internal/cmd/dns/util.ProjectControlPlaneURL performs.
func projectControlPlanePath(project string) string {
	return fmt.Sprintf("/apis/%s/%s/projects/%s/control-plane", resourceManagerGroup, resourceManagerVersion, project)
}

// clientConfig derives the REST config one request reads through: the
// caller's token, pointed at their project control plane. The base config
// supplies the endpoint and CA only — every credential field it might carry
// is cleared first, so the server's own identity can never leak into a
// caller's read.
func clientConfig(baseConfig *rest.Config, token, project string) (*rest.Config, error) {
	// The project arrives in a header and is interpolated into a URL path,
	// so it is validated before it can reshape that path into another API
	// route.
	if errs := validation.IsDNS1123Subdomain(project); len(errs) > 0 {
		return nil, fmt.Errorf("invalid project %q on the %s header sent by the client that called "+
			"this tool: %s. %s", project, projectHeader, strings.Join(errs, "; "), misconfiguredClientNote)
	}

	cfg := rest.AnonymousClientConfig(rest.CopyConfig(baseConfig))
	cfg.BearerToken = token
	cfg.BearerTokenFile = ""

	host, err := url.Parse(cfg.Host)
	if err != nil {
		return nil, fmt.Errorf("parsing control plane host: %w", err)
	}
	host.Path = projectControlPlanePath(project)
	cfg.Host = host.String()

	return cfg, nil
}

// clientForToken builds a client that reads project's control plane as the
// bearer of token.
func clientForToken(baseConfig *rest.Config, token, project string) (client.Client, error) {
	cfg, err := clientConfig(baseConfig, token, project)
	if err != nil {
		return nil, err
	}

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("building client for caller: %w", err)
	}
	return c, nil
}

// bearerToken extracts a bearer token from the Authorization header.
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if len(auth) < len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(auth[len(prefix):])
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
