// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/transport"
)

type goneSource struct{ calls int }

func (s *goneSource) Next(context.Context) (transport.Message, error) {
	s.calls++
	return nil, fmt.Errorf("%w: deleted", transport.ErrConsumerGone)
}

func TestConsumeReturnsWhenItsDurableIsDeleted(t *testing.T) {
	agent := &Agent{cfg: Config{RetryDelay: time.Millisecond}}
	source := &goneSource{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := agent.consume(ctx, source)
	if !errors.Is(err, transport.ErrConsumerGone) {
		t.Fatalf("consume returned %v, want ErrConsumerGone so the transport recreates the durable", err)
	}
	if source.calls != 1 {
		t.Fatalf("consume pulled %d times from a deleted durable, want 1", source.calls)
	}
}
