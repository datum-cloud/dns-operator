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
	flag.StringVar(&role, "role", "", "Process role: control-plane, agent, or watchdog")
	flag.StringVar(&configPath, "config", "", "Absolute path to the internal DNS JSON configuration")
	// controller-runtime/client-go may install this standard flag from an init
	// hook. Reuse it when present so the command keeps one stable contract.
	if flag.Lookup("kubeconfig") == nil {
		flag.StringVar(&kubeconfig, "kubeconfig", "", "Control-plane Kubernetes configuration; ignored by agent and watchdog")
	}
	logOptions := zap.Options{Development: false}
	logOptions.BindFlags(flag.CommandLine)
	flag.Parse()
	if existing := flag.Lookup("kubeconfig"); existing != nil {
		kubeconfig = existing.Value.String()
	}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&logOptions)))

	cfg, err := internruntime.LoadConfig(configPath)
	if err == nil {
		err = internruntime.Run(ctrl.SetupSignalHandler(), role, cfg, kubeconfig)
	}
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "internal-dns: %v\n", err)
		os.Exit(1)
	}
}
