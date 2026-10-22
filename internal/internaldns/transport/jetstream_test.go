package transport

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

func TestProductionTransportRequiresTLSAndOneAuthMethod(t *testing.T) {
	t.Parallel()
	base := Config{URL: "nats://example:4222", Name: "agent"}
	if err := base.validate(); err == nil {
		t.Fatal("plaintext production NATS accepted")
	}
	base.TLS = &TLSConfig{ServerName: "nats.example"}
	if err := base.validate(); err == nil {
		t.Fatal("unauthenticated production NATS accepted")
	}
	base.Token = "secret"
	if err := base.validate(); err != nil {
		t.Fatal(err)
	}
	base.Username, base.Password = "also", "configured"
	if err := base.validate(); err == nil {
		t.Fatal("multiple authentication methods accepted")
	}
}

func TestDevelopmentTransportMustBeExplicit(t *testing.T) {
	t.Parallel()
	if err := (Config{URL: "nats://127.0.0.1:4222", Name: "e2e", AllowInsecure: true}).validate(); err != nil {
		t.Fatal(err)
	}
}

func TestDurableNameRejectsQueueSharingSyntax(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"", "replica.one", "replica one", "replica*"} {
		if validDurable(bad) {
			t.Fatalf("unsafe durable %q accepted", bad)
		}
	}
	if !validDurable("auth-replica-one") {
		t.Fatal("safe durable rejected")
	}
}

func TestBrokerMessageIDIsNamespacedBySubject(t *testing.T) {
	t.Parallel()
	eventID := "event-123"
	publication := brokerMessageID("internaldns.pub.us-east.s1.manifest.zone-a", eventID)
	ack := brokerMessageID("internaldns.ack.us-east.s1.member-a", eventID)
	if publication == ack {
		t.Fatal("the same event ID collided across independently authorized subjects")
	}
	if publication != brokerMessageID("internaldns.pub.us-east.s1.manifest.zone-a", eventID) {
		t.Fatal("message ID is not stable for an ambiguous retry")
	}
}

type nextResultConsumer struct {
	jetstream.Consumer
	next func() (jetstream.Msg, error)
}

func (c nextResultConsumer) Next(...jetstream.FetchOpt) (jetstream.Msg, error) {
	return c.next()
}

func TestConsumerWaitsThroughIdlePulls(t *testing.T) {
	for _, finalError := range []error{jetstream.ErrNoHeartbeat, nats.ErrConnectionClosed} {
		t.Run(finalError.Error(), func(t *testing.T) {
			calls := 0
			c := Consumer{c: nextResultConsumer{next: func() (jetstream.Msg, error) {
				calls++
				if calls == 1 {
					return nil, nats.ErrTimeout
				}
				return nil, finalError
			}}}
			_, err := c.Next(context.Background())
			if !errors.Is(err, finalError) || calls != 2 {
				t.Fatalf("got %v after %d pulls; want %v after idle pull", err, calls, finalError)
			}
		})
	}
}

func TestConsumerIdlePullHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	c := Consumer{c: nextResultConsumer{next: func() (jetstream.Msg, error) {
		calls++
		cancel()
		return nil, nats.ErrTimeout
	}}}
	_, err := c.Next(ctx)
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("got %v after %d pulls; want cancellation", err, calls)
	}
}

type envelopeMessage struct {
	jetstream.Msg
	data       []byte
	terminated bool
	retryDelay time.Duration
}

func (m *envelopeMessage) Data() []byte { return m.data }
func (m *envelopeMessage) TermWithReason(string) error {
	m.terminated = true
	return nil
}
func (m *envelopeMessage) NakWithDelay(delay time.Duration) error {
	m.retryDelay = delay
	return nil
}

type envelopeConsumer struct {
	jetstream.Consumer
	message jetstream.Msg
}

func (c envelopeConsumer) Next(...jetstream.FetchOpt) (jetstream.Msg, error) {
	return c.message, nil
}

func TestUnsupportedEnvelopesRemainDeliverable(t *testing.T) {
	for _, kind := range []model.Kind{model.KindMemberAck, "FutureKind"} {
		env := model.Envelope{APIVersion: "future-version", Kind: kind, EventID: "event", Region: "east", Shard: "shared", ResourceUID: "resource", Epoch: 1, Revision: 1, PublishedAt: time.Now(), Payload: json.RawMessage(`{}`)}
		if kind != model.KindMemberAck {
			env.APIVersion = model.APIVersion
		}
		data, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		message := &envelopeMessage{data: data}
		c := Consumer{c: envelopeConsumer{message: message}}
		if _, err := c.Next(context.Background()); !errors.Is(err, model.ErrUnsupportedEnvelope) || message.terminated || message.retryDelay != time.Minute {
			t.Fatalf("unsupported event not retained: error=%v terminated=%t retry=%v", err, message.terminated, message.retryDelay)
		}
	}
}

func TestMalformedEnvelopesAreTerminated(t *testing.T) {
	for _, data := range []string{`invalid-json`, `{"apiVersion":"future","kind":"FutureKind"}`} {
		message := &envelopeMessage{data: []byte(data)}
		c := Consumer{c: envelopeConsumer{message: message}}
		if _, err := c.Next(context.Background()); err == nil || !message.terminated || message.retryDelay != 0 {
			t.Fatalf("malformed event not terminated: error=%v terminated=%t retry=%v", err, message.terminated, message.retryDelay)
		}
	}
}

type emptyKeyValue struct{ jetstream.KeyValue }

func (emptyKeyValue) Keys(context.Context, ...jetstream.WatchOpt) ([]string, error) {
	return nil, jetstream.ErrNoKeysFound
}

func TestSnapshotListOfEmptyBucketIsEmpty(t *testing.T) {
	envelopes, err := (&SnapshotStore{kv: emptyKeyValue{}}).List(context.Background(), "central.")
	if err != nil || len(envelopes) != 0 {
		t.Fatalf("empty bucket must list no snapshots, got %v, %v", envelopes, err)
	}
}
