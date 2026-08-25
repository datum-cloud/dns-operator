// SPDX-License-Identifier: AGPL-3.0-only

package plugin_test

import (
	"crypto/x509"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"k8s.io/client-go/rest"
)

// controlPlanePath matches the URL shape util.ProjectControlPlaneURL builds.
// The project is captured rather than fixed so the proxy can serve more than
// one, and so a test can assert which project the plugin actually addressed.
var controlPlanePath = regexp.MustCompile(
	`^/apis/resourcemanager\.miloapis\.com/v1alpha1/projects/([^/]+)/control-plane`)

// controlPlaneProxy presents an envtest API server at a Datum project
// control-plane URL.
//
// This is the seam that lets the harness exercise the real client construction
// path. util.NewClient builds
// https://<APIHost>/apis/resourcemanager.miloapis.com/v1alpha1/projects/<p>/control-plane
// and authenticates with a bearer token; envtest serves a bare apiserver at
// https://127.0.0.1:<port> and authenticates with a client certificate. Rather
// than teach the plugin about that difference — which would mean testing a code
// path production never takes — the proxy absorbs it: it accepts the
// production URL and the bearer token, strips the project prefix, and forwards
// to envtest over envtest's own credentials.
//
// The one production change this requires is DATUM_CA_FILE (util.CAFileEnv), so
// the plugin will trust the proxy's self-signed certificate. TLS is genuinely
// verified against that certificate; nothing is skipped.
type controlPlaneProxy struct {
	server *httptest.Server

	mu       sync.Mutex
	requests []proxiedRequest
}

// proxiedRequest is one observed call, recorded so tests can assert on what the
// plugin sent rather than only on what came back.
type proxiedRequest struct {
	Method    string
	Path      string
	Project   string
	Token     string
	UserAgent string
	Matched   bool
}

// newControlPlaneProxy starts a TLS proxy in front of the given API server.
func newControlPlaneProxy(target *rest.Config) (*controlPlaneProxy, error) {
	upstream, err := url.Parse(target.Host)
	if err != nil {
		return nil, fmt.Errorf("parsing envtest host %q: %w", target.Host, err)
	}

	// Forward over envtest's own transport, which carries its client
	// certificate and CA.
	transport, err := rest.TransportFor(target)
	if err != nil {
		return nil, fmt.Errorf("building upstream transport: %w", err)
	}

	p := &controlPlaneProxy{}

	reverse := &httputil.ReverseProxy{
		Transport: transport,
		Director: func(r *http.Request) {
			r.URL.Scheme = upstream.Scheme
			r.URL.Host = upstream.Host
			r.Host = upstream.Host
			// The plugin's bearer token means nothing upstream, and leaving it
			// in place would make the apiserver attempt token auth and reject
			// the request before it considers the client certificate.
			r.Header.Del("Authorization")
		},
	}

	p.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		match := controlPlanePath.FindStringSubmatch(r.URL.Path)

		observed := proxiedRequest{
			Method:    r.Method,
			Path:      r.URL.Path,
			Token:     strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "),
			UserAgent: r.Header.Get("User-Agent"),
			Matched:   match != nil,
		}
		if match != nil {
			observed.Project = match[1]
		}
		p.record(observed)

		if match == nil {
			// Not a project control-plane URL. Failing loudly beats silently
			// proxying, which would let a wrong URL shape pass as working.
			http.Error(w, "proxy: not a project control-plane path: "+r.URL.Path, http.StatusBadGateway)
			return
		}

		r.URL.Path = strings.TrimPrefix(r.URL.Path, match[0])
		if r.URL.Path == "" {
			r.URL.Path = "/"
		}
		reverse.ServeHTTP(w, r)
	}))

	return p, nil
}

func (p *controlPlaneProxy) record(r proxiedRequest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, r)
}

// Requests returns a copy of everything observed so far.
func (p *controlPlaneProxy) Requests() []proxiedRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]proxiedRequest(nil), p.requests...)
}

// Reset clears the request log, so a test can assert on its own traffic.
func (p *controlPlaneProxy) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = nil
}

// HostPort is the value DATUM_API_HOST takes: host:port with no scheme, since
// util.ProjectControlPlaneURL supplies the https:// itself.
func (p *controlPlaneProxy) HostPort() string {
	return strings.TrimPrefix(p.server.URL, "https://")
}

// Certificate is the proxy's self-signed certificate, which the plugin is
// pointed at through DATUM_CA_FILE.
func (p *controlPlaneProxy) Certificate() *x509.Certificate {
	return p.server.Certificate()
}

func (p *controlPlaneProxy) Close() {
	p.server.Close()
}
