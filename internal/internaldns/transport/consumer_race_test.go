// SPDX-License-Identifier: AGPL-3.0-only

package transport

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
)

// failingIter is a message iterator whose every Next fails, as one does while
// its connection closes.
type failingIter struct{}

func (failingIter) Next(...jetstream.NextOpt) (jetstream.Msg, error) {
	return nil, errors.New("iterator closed")
}
func (failingIter) Stop()  {}
func (failingIter) Drain() {}

type iterConsumer struct{ jetstream.Consumer }

func (iterConsumer) Messages(...jetstream.PullMessagesOpt) (jetstream.MessagesContext, error) {
	return failingIter{}, nil
}

func TestConcurrentPullsSurviveAFailingIterator(t *testing.T) {
	c := &Consumer{c: iterConsumer{}, prefetch: 32}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 500 {
				if _, err := c.pull(context.Background()); err == nil {
					t.Error("a failing iterator returned a message")
					return
				}
			}
		}()
	}
	wg.Wait()
}
