package config

import (
	multiclusterproviders "go.miloapis.com/milo/pkg/multicluster-runtime"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	ctrl "sigs.k8s.io/controller-runtime"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:defaulter-gen=true

type DNSOperator struct {
	metav1.TypeMeta `json:",inline"`

	Discovery DiscoveryConfig `json:"discovery"`

	DownstreamResourceManagement DownstreamResourceManagementConfig `json:"downstreamResourceManagement"`

	Controllers ControllersConfig `json:"controllers"`
}

// +k8s:deepcopy-gen=true
type ControllersConfig struct {
	DNSRecordSetPowerDNS DNSRecordSetPowerDNSControllerConfig `json:"dnsRecordSetPowerDNS"`
}

// +k8s:deepcopy-gen=true
type DNSRecordSetPowerDNSControllerConfig struct {
	// MaxConcurrentReconciles is the maximum number of concurrent reconciles for the
	// dnsrecordset-powerdns controller.
	//
	// +default=4
	MaxConcurrentReconciles int `json:"maxConcurrentReconciles"`

	// RateLimiterBaseDelay is the base delay for the exponential failure rate limiter.
	//
	// +default="1s"
	RateLimiterBaseDelay *metav1.Duration `json:"rateLimiterBaseDelay"`

	// RateLimiterMaxDelay is the maximum delay for the exponential failure rate limiter.
	//
	// +default="30s"
	RateLimiterMaxDelay *metav1.Duration `json:"rateLimiterMaxDelay"`
}

// +k8s:deepcopy-gen=true

type DiscoveryConfig struct {
	// Mode is the mode that the operator should use to discover clusters.
	//
	// Defaults to "single"
	Mode multiclusterproviders.Provider `json:"mode"`

	// InternalServiceDiscovery will result in the operator to connect to internal
	// service addresses for projects.
	InternalServiceDiscovery bool `json:"internalServiceDiscovery"`

	// DiscoveryKubeconfigPath is the path to the kubeconfig file to use for
	// project discovery. When not provided, the operator will use the in-cluster
	// config.
	DiscoveryKubeconfigPath string `json:"discoveryKubeconfigPath"`

	// ProjectKubeconfigPath is the path to the kubeconfig file to use as a
	// template when connecting to project control planes. When not provided,
	// the operator will use the in-cluster config.
	ProjectKubeconfigPath string `json:"projectKubeconfigPath"`
}

func (c *DiscoveryConfig) DiscoveryRestConfig() (*rest.Config, error) {
	if c.DiscoveryKubeconfigPath == "" {
		return ctrl.GetConfig()
	}

	return clientcmd.BuildConfigFromFlags("", c.DiscoveryKubeconfigPath)
}

func (c *DiscoveryConfig) ProjectRestConfig() (*rest.Config, error) {
	if c.ProjectKubeconfigPath == "" {
		return ctrl.GetConfig()
	}

	return clientcmd.BuildConfigFromFlags("", c.ProjectKubeconfigPath)
}

// +k8s:deepcopy-gen=true

type DownstreamResourceManagementConfig struct {
	// DNSZoneAccountingNamespace is the namespace where the DNSZone accounting is performed.
	//
	// +default="datum-downstream-dnszone-accounting"
	DNSZoneAccountingNamespace string `json:"dnsZoneAccountingNamespace"`

	// KubeconfigPath is the path to the kubeconfig file to use when managing
	// downstream resources. When not provided, the operator will use the
	// in-cluster config.
	KubeconfigPath string `json:"kubeconfigPath"`
}

func (c *DownstreamResourceManagementConfig) RestConfig() (*rest.Config, error) {
	if c.KubeconfigPath == "" {
		return ctrl.GetConfig()
	}

	return clientcmd.BuildConfigFromFlags("", c.KubeconfigPath)
}
