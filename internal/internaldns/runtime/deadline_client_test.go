// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

type blockedSourceClient struct{ client.Client }

func (blockedSourceClient) List(ctx context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestDeadlineClientBoundsPartitionedSourceRequests(t *testing.T) {
	c := &deadlineClient{Client: blockedSourceClient{}, Timeout: 25 * time.Millisecond}
	started := time.Now()
	err := c.List(context.Background(), nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("partitioned source request returned %v", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("partitioned source request exceeded its bound: %s", elapsed)
	}
}
