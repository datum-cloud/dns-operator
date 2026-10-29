package controller

import (
	"context"
	"errors"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var errDownstreamTerminating = errors.New("downstream resource or namespace is terminating")

func checkDownstreamNamespace(ctx context.Context, c client.Client, name string) error {
	var namespace corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: name}, &namespace); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if !namespace.DeletionTimestamp.IsZero() {
		return errDownstreamTerminating
	}
	return nil
}

// Admission can observe namespace deletion after our cache-based preflight.
// Only this specific lifecycle refusal is expected; preserve RBAC failures.
func downstreamTerminationError(err error) error {
	if apierrors.IsForbidden(err) && apierrors.HasStatusCause(err, corev1.NamespaceTerminatingCause) {
		return errDownstreamTerminating
	}
	return err
}
