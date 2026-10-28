// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"flag"
	"fmt"
	"os"

	internruntime "go.miloapis.com/dns-operator/internal/internaldns/runtime"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

func main() {
	var role, configPath, kubeconfig string
	var opts internruntime.Options
	flag.StringVar(&role, "role", "", "Process role: control-plane, agent, serving, readiness, or prepare")
	flag.StringVar(&configPath, "config", "", "Absolute path to the internal DNS JSON configuration")
	flag.StringVar(&opts.MetricsBindAddress, "metrics-bind-address", ":8080", "Control-plane or agent metrics listener, or 0 to disable; the configuration's metricsBindAddress overrides it")
	flag.StringVar(&opts.HealthProbeBindAddress, "health-probe-bind-address", ":8081", "Control-plane /healthz and /readyz listener, or 0 to disable")
	// controller-runtime/client-go may install this standard flag from an init
	// hook. Reuse it when present so the command keeps one stable contract.
	if flag.Lookup("kubeconfig") == nil {
		flag.StringVar(&kubeconfig, "kubeconfig", "", "Control-plane Kubernetes configuration; ignored by serving processes")
	}
	logOptions := zap.Options{Development: false}
	logOptions.BindFlags(flag.CommandLine)
	flag.Parse()
	if existing := flag.Lookup("kubeconfig"); existing != nil {
		kubeconfig = existing.Value.String()
	}
	opts.Kubeconfig = kubeconfig
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&logOptions)))

	cfg, err := internruntime.LoadConfig(configPath)
	if err == nil {
		err = internruntime.Run(ctrl.SetupSignalHandler(), role, cfg, opts)
	}
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "internal-dns: %v\n", err)
		os.Exit(1)
	}
}
