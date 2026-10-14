// SPDX-License-Identifier: AGPL-3.0-only

// Package dnsagent is the importable surface of DNS's agent tools, for an MCP
// server outside this module that wants to offer them — datum-mcp, which runs
// on a customer's own machine and so cannot reach the in-cluster dns-mcp.
//
// Everything here is a thin re-export of internal/agent. The diagnosis walk,
// the reason catalog and the rendering stay internal and singular, so a second
// server mounting these tools describes a zone exactly the way the assistant
// and the dns plugin do. Do not add logic here; add it to internal/agent and
// re-export it.
//
// The knowledge and skills that go with these tools are already importable from
// go.miloapis.com/dns-operator/docs/agent.
package dnsagent

import (
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/agent"
)

// The tool names RegisterTools adds. Each carries the dns_ prefix so they can
// sit beside another server's tools without colliding.
const (
	ToolZonesList        = agent.ToolZonesList
	ToolZonesGet         = agent.ToolZonesGet
	ToolZoneDiagnose     = agent.ToolZoneDiagnose
	ToolDelegationCheck  = agent.ToolDelegationCheck
	ToolRecordsList      = agent.ToolRecordsList
	ToolRecordsGet       = agent.ToolRecordsGet
	ToolRecordDiagnose   = agent.ToolRecordDiagnose
	ToolRecordRender     = agent.ToolRecordRender
	ToolZoneDiscoveryGet = agent.ToolZoneDiscoveryGet
)

// Namespace is the namespace every DNS object lives in within a project
// control plane.
const Namespace = "default"

type (
	// Reader fetches the objects a zone is assembled from. It has no write
	// method, by design; see internal/agent.Reader.
	Reader = agent.Reader
	// ToolDeps is what one request's tool calls read through.
	ToolDeps = agent.ToolDeps
	// DepsFor resolves ToolDeps per call, so the caller decides how identity
	// and project are established.
	DepsFor = agent.DepsFor
)

// RegisterTools adds every DNS tool to s. None of them writes.
func RegisterTools(s *mcp.Server, deps DepsFor) {
	agent.RegisterTools(s, deps)
}

// NewClientReader returns a Reader backed by c, which must be built with a
// scheme from NewScheme. Every read runs with whatever credentials c carries.
func NewClientReader(c client.Client) Reader {
	return agent.NewClientReader(c)
}

// NewScheme returns a scheme registering every type a Reader decodes.
func NewScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("registering core scheme: %w", err)
	}
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("registering dns scheme: %w", err)
	}
	return scheme, nil
}
