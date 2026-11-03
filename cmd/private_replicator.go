// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	"go.miloapis.com/dns-operator/internal/config"
	"go.miloapis.com/dns-operator/internal/privatereplication"
)

var projectGVK = schema.GroupVersionKind{Group: "resourcemanager.miloapis.com", Version: "v1alpha1", Kind: "Project"}

// runPrivateReplicator copies every project's private DNS objects into the
// federation hub and mirrors region status and region writes back.
func runPrivateReplicator(serverConfig config.DNSOperator, hubKubeconfig string, opts ctrl.Options) error {
	if hubKubeconfig == "" {
		return errors.New("--private-hub-kubeconfig is required for the private-replicator role")
	}
	hubConfig, err := clientcmd.BuildConfigFromFlags("", hubKubeconfig)
	if err != nil {
		return fmt.Errorf("load hub kubeconfig: %w", err)
	}
	hub, err := cluster.New(hubConfig, func(o *cluster.Options) { o.Scheme = opts.Scheme })
	if err != nil {
		return fmt.Errorf("create hub cluster: %w", err)
	}
	cfg := ctrl.GetConfigOrDie()
	deploymentCluster, err := cluster.New(cfg, func(o *cluster.Options) { o.Scheme = opts.Scheme })
	if err != nil {
		return fmt.Errorf("create local cluster: %w", err)
	}
	runnables, mcmgr, err := newReplicatorManager(serverConfig, cfg, deploymentCluster, opts)
	if err != nil {
		return err
	}
	discoveryConfig, err := serverConfig.Discovery.DiscoveryRestConfig()
	if err != nil {
		return fmt.Errorf("load discovery config: %w", err)
	}
	projects, err := client.New(discoveryConfig, client.Options{})
	if err != nil {
		return fmt.Errorf("create project client: %w", err)
	}
	projectUID := func(ctx context.Context, name multicluster.ClusterName) (types.UID, error) {
		project := &unstructured.Unstructured{}
		project.SetGroupVersionKind(projectGVK)
		parts := strings.Split(string(name), "/")
		if err := projects.Get(ctx, client.ObjectKey{Name: parts[len(parts)-1]}, project); err != nil {
			return "", err
		}
		return project.GetUID(), nil
	}
	for _, kind := range privatereplication.Kinds {
		if err := (&privatereplication.Replicator{GVK: kind, Hub: hub.GetClient(), ProjectUID: projectUID}).SetupWithManager(mcmgr); err != nil {
			return fmt.Errorf("set up %s replicator: %w", kind.Kind, err)
		}
	}
	if err := (&privatereplication.StatusMirror{Hub: hub.GetClient()}).SetupWithManager(mcmgr, hub); err != nil {
		return fmt.Errorf("set up status mirror: %w", err)
	}
	for _, kind := range privatereplication.RegionWritable {
		if err := (&privatereplication.ObjectMirror{GVK: kind, Hub: hub.GetClient()}).SetupWithManager(mcmgr, hub); err != nil {
			return fmt.Errorf("set up %s mirror: %w", kind.Kind, err)
		}
	}
	return runReplicator(ctrl.SetupSignalHandler(), runnables, hub, mcmgr)
}
