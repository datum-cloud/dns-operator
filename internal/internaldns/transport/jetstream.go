// Package transport provides the authenticated, durable regional event bus for
// internal DNS. Every serving member must use a
// different Durable name; sharing it would divide updates between replicas.
package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

type TLSConfig struct {
	CAFile             string `json:"caFile,omitempty"`
	CertificateFile    string `json:"certificateFile,omitempty"`
	KeyFile            string `json:"keyFile,omitempty"`
	ServerName         string `json:"serverName,omitempty"`
	InsecureSkipVerify bool   `json:"insecureSkipVerify,omitempty"`
}

type Config struct {
	URL             string     `json:"url"`
	Name            string     `json:"name"`
	Username        string     `json:"username,omitempty"`
	Password        string     `json:"password,omitempty"`
	PasswordFile    string     `json:"passwordFile,omitempty"`
	Token           string     `json:"token,omitempty"`
	CredentialsFile string     `json:"credentialsFile,omitempty"`
	TLS             *TLSConfig `json:"tls,omitempty"`
	// AllowInsecure is intended for isolated development fixtures only. A
	// production caller must configure TLS and an authentication method.
	AllowInsecure  bool          `json:"allowInsecure,omitempty"`
	ConnectTimeout time.Duration `json:"connectTimeout,omitempty"`
	PublishTimeout time.Duration `json:"publishTimeout,omitempty"`
}

func (c Config) validate() error {
	if c.URL == "" || c.Name == "" {
		return errors.New("NATS URL and connection name are required")
	}
	if !c.AllowInsecure && c.TLS == nil {
		return errors.New("NATS TLS is required unless AllowInsecure is explicitly set")
	}
	if c.Password != "" && c.PasswordFile != "" {
		return errors.New("NATS password and password file are mutually exclusive")
	}
	if (c.Password != "" || c.PasswordFile != "") && c.Username == "" {
		return errors.New("NATS username is required with a password source")
	}
	if c.Username != "" && c.Password == "" && c.PasswordFile == "" {
		return errors.New("NATS username requires a password source")
	}
	if c.AllowInsecure && c.CredentialsFile == "" && c.Token == "" && c.Username == "" {
		return nil
	}
	auth := 0
	if c.CredentialsFile != "" {
		auth++
	}
	if c.Token != "" {
		auth++
	}
	if c.Username != "" {
		auth++
	}
	if auth != 1 {
		return errors.New("exactly one NATS authentication method is required")
	}
	return nil
}

type JetStream struct {
	nc             *nats.Conn
	js             jetstream.JetStream
	publishTimeout time.Duration
}

func Connect(c Config) (*JetStream, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	opts := []nats.Option{
		nats.Name(c.Name),
		nats.Timeout(defaultDuration(c.ConnectTimeout, 10*time.Second)),
		// A regional partition must not permanently close a long-running agent.
		// Callers keep retrying while local expiry and the watchdog gate serving.
		nats.MaxReconnects(-1),
	}
	if c.CredentialsFile != "" {
		opts = append(opts, nats.UserCredentials(c.CredentialsFile))
	}
	if c.Token != "" {
		opts = append(opts, nats.Token(c.Token))
	}
	if c.Username != "" {
		password := c.Password
		if c.PasswordFile != "" {
			value, err := os.ReadFile(c.PasswordFile)
			if err != nil {
				return nil, fmt.Errorf("read NATS password: %w", err)
			}
			password = strings.TrimSpace(string(value))
			if password == "" {
				return nil, errors.New("NATS password file is empty")
			}
		}
		opts = append(opts, nats.UserInfo(c.Username, password))
	}
	if c.TLS != nil {
		tlsConfig, err := loadTLS(*c.TLS)
		if err != nil {
			return nil, err
		}
		opts = append(opts, nats.Secure(tlsConfig))
	}
	nc, err := nats.Connect(c.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("connect NATS: %w", err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("open JetStream: %w", err)
	}
	return &JetStream{nc: nc, js: js, publishTimeout: defaultDuration(c.PublishTimeout, 5*time.Second)}, nil
}

func loadTLS(c TLSConfig) (*tls.Config, error) {
	if c.InsecureSkipVerify {
		return nil, errors.New("NATS TLS InsecureSkipVerify is forbidden")
	}
	t := &tls.Config{MinVersion: tls.VersionTLS13, ServerName: c.ServerName}
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read NATS CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("NATS CA file contains no certificates")
		}
		t.RootCAs = pool
	}
	if c.CertificateFile != "" || c.KeyFile != "" {
		if c.CertificateFile == "" || c.KeyFile == "" {
			return nil, errors.New("NATS client certificate and key must be configured together")
		}
		cert, err := tls.LoadX509KeyPair(c.CertificateFile, c.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load NATS client certificate: %w", err)
		}
		t.Certificates = []tls.Certificate{cert}
	}
	return t, nil
}

func (j *JetStream) Close() error {
	if j == nil || j.nc == nil {
		return nil
	}
	if err := j.nc.Drain(); err != nil {
		j.nc.Close()
		return err
	}
	return nil
}

type PublishAck struct {
	Stream    string
	Sequence  uint64
	Duplicate bool
}

// ExportPublisher is implemented by JetStream and used by the durable outbox
// controller. EventID supplies JetStream de-duplication for ambiguous retries.
type ExportPublisher interface {
	Publish(context.Context, string, model.Envelope) (PublishAck, error)
}

func (j *JetStream) Publish(ctx context.Context, subject string, env model.Envelope) (PublishAck, error) {
	if err := env.Validate(); err != nil {
		return PublishAck{}, err
	}
	if subject == "" {
		return PublishAck{}, errors.New("publish subject is required")
	}
	if j == nil || j.nc == nil || j.nc.IsClosed() {
		return PublishAck{}, nats.ErrConnectionClosed
	}
	if !j.nc.IsConnected() {
		return PublishAck{}, nats.ErrDisconnected
	}
	b, err := json.Marshal(env)
	if err != nil {
		return PublishAck{}, err
	}
	publishCtx, cancel := boundedContext(ctx, defaultDuration(j.publishTimeout, 5*time.Second))
	defer cancel()
	ack, err := j.js.Publish(publishCtx, subject, b, jetstream.WithMsgID(brokerMessageID(subject, env.EventID)))
	if err != nil {
		return PublishAck{}, fmt.Errorf("publish %s: %w", subject, err)
	}
	return PublishAck{Stream: ack.Stream, Sequence: ack.Sequence, Duplicate: ack.Duplicate}, nil
}

func brokerMessageID(subject, eventID string) string {
	// JetStream de-duplication spans every subject in a stream. Namespace the
	// application event ID so an ACK publisher cannot reserve a controller
	// publication's ID on its independently authorized subject.
	return model.Hash([]byte(subject + "\x00" + eventID))
}

func boundedContext(parent context.Context, limit time.Duration) (context.Context, context.CancelFunc) {
	if deadline, ok := parent.Deadline(); ok && time.Until(deadline) <= limit {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, limit)
}

type StreamConfig struct {
	Name            string        `json:"name"`
	Subjects        []string      `json:"subjects,omitempty"`
	MaxAge          time.Duration `json:"maxAge,omitempty"`
	MaxBytes        int64         `json:"maxBytes,omitempty"`
	Replicas        int           `json:"replicas,omitempty"`
	SnapshotsBucket string        `json:"snapshotsBucket"`
}

func (j *JetStream) EnsureStream(ctx context.Context, c StreamConfig) error {
	if c.Name == "" || len(c.Subjects) == 0 {
		return errors.New("stream name and subjects are required")
	}
	_, err := j.js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{Name: c.Name, Subjects: c.Subjects, Retention: jetstream.LimitsPolicy, Storage: jetstream.FileStorage, MaxAge: c.MaxAge, MaxBytes: c.MaxBytes, Replicas: c.Replicas, Duplicates: 2 * time.Minute})
	if err != nil {
		return fmt.Errorf("ensure stream %q: %w", c.Name, err)
	}
	if c.SnapshotsBucket != "" {
		_, err = j.js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: c.SnapshotsBucket, Description: "current complete internal DNS bootstrap snapshots", History: 1, Storage: jetstream.FileStorage, Replicas: c.Replicas})
		if err != nil {
			return fmt.Errorf("ensure snapshot bucket %q: %w", c.SnapshotsBucket, err)
		}
	}
	return nil
}

type ConsumerConfig struct {
	Stream         string        `json:"stream"`
	Durable        string        `json:"durable"`
	FilterSubjects []string      `json:"filterSubjects"`
	AckWait        time.Duration `json:"ackWait,omitempty"`
	MaxAckPending  int           `json:"maxAckPending,omitempty"`
}

type Message interface {
	Subject() string
	Envelope() model.Envelope
	StreamSequence() uint64
	Ack(context.Context) error
	Retry(time.Duration) error
	Reject(string) error
}

type Consumer struct{ c jetstream.Consumer }

func (j *JetStream) Consumer(ctx context.Context, c ConsumerConfig) (*Consumer, error) {
	if c.Stream == "" || !validDurable(c.Durable) || len(c.FilterSubjects) == 0 {
		return nil, errors.New("stream, safe durable name, and filters are required")
	}
	cons, err := j.js.CreateOrUpdateConsumer(ctx, c.Stream, jetstream.ConsumerConfig{Durable: c.Durable, AckPolicy: jetstream.AckExplicitPolicy, DeliverPolicy: jetstream.DeliverAllPolicy, ReplayPolicy: jetstream.ReplayInstantPolicy, FilterSubjects: c.FilterSubjects, AckWait: defaultDuration(c.AckWait, 30*time.Second), MaxAckPending: defaultInt(c.MaxAckPending, 64)})
	if err != nil {
		return nil, fmt.Errorf("ensure durable consumer %q: %w", c.Durable, err)
	}
	return &Consumer{c: cons}, nil
}

// Next returns one event. Malformed envelopes are terminated so a poison event
// cannot block every later revision on an ordered durable.
func (c *Consumer) Next(ctx context.Context) (Message, error) {
	m, err := c.c.Next(jetstream.FetchContext(ctx))
	if err != nil {
		return nil, err
	}
	var env model.Envelope
	if err := json.Unmarshal(m.Data(), &env); err != nil {
		_ = m.TermWithReason("invalid JSON envelope")
		return nil, fmt.Errorf("decode envelope: %w", err)
	}
	if err := env.Validate(); err != nil {
		_ = m.TermWithReason("invalid DNS envelope")
		return nil, err
	}
	meta, err := m.Metadata()
	if err != nil {
		_ = m.Nak()
		return nil, err
	}
	return &message{msg: m, subject: m.Subject(), env: env, seq: meta.Sequence.Stream}, nil
}

type message struct {
	msg     jetstream.Msg
	subject string
	env     model.Envelope
	seq     uint64
}

func (m *message) Subject() string               { return m.subject }
func (m *message) Envelope() model.Envelope      { return m.env }
func (m *message) StreamSequence() uint64        { return m.seq }
func (m *message) Ack(ctx context.Context) error { return m.msg.DoubleAck(ctx) }
func (m *message) Retry(d time.Duration) error   { return m.msg.NakWithDelay(d) }
func (m *message) Reject(reason string) error    { return m.msg.TermWithReason(reason) }

type SnapshotStore struct{ kv jetstream.KeyValue }

func (j *JetStream) SnapshotStore(ctx context.Context, bucket string) (*SnapshotStore, error) {
	if bucket == "" {
		return nil, errors.New("snapshot bucket is required")
	}
	kv, err := j.js.KeyValue(ctx, bucket)
	if err != nil {
		return nil, fmt.Errorf("open snapshot bucket %q: %w", bucket, err)
	}
	return &SnapshotStore{kv: kv}, nil
}

// Put stores a complete, already validated bootstrap envelope. It uses a KV
// compare-and-swap loop so a delayed publisher cannot replace a current
// bootstrap pointer with an older epoch or revision.
func (s *SnapshotStore) Put(ctx context.Context, key string, env model.Envelope) (uint64, error) {
	if err := env.Validate(); err != nil {
		return 0, err
	}
	if !validSnapshotKey(key) {
		return 0, fmt.Errorf("invalid snapshot key %q", key)
	}
	b, err := json.Marshal(env)
	if err != nil {
		return 0, err
	}
	for attempt := 0; attempt < 8; attempt++ {
		current, err := s.kv.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			revision, err := s.kv.Create(ctx, key, b)
			if errors.Is(err, jetstream.ErrKeyExists) || errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
				continue
			}
			return revision, err
		}
		if err != nil {
			return 0, err
		}
		var old model.Envelope
		if err := json.Unmarshal(current.Value(), &old); err != nil {
			return 0, fmt.Errorf("decode current snapshot %q: %w", key, err)
		}
		cmp := model.Compare(env.Epoch, env.Revision, old.Epoch, old.Revision)
		if cmp < 0 {
			return current.Revision(), nil
		}
		if cmp == 0 {
			if model.Hash(current.Value()) != model.Hash(b) {
				return 0, fmt.Errorf("snapshot %q has conflicting content at epoch %d revision %d", key, env.Epoch, env.Revision)
			}
			return current.Revision(), nil
		}
		revision, err := s.kv.Update(ctx, key, b, current.Revision())
		if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
			continue
		}
		return revision, err
	}
	return 0, fmt.Errorf("snapshot %q CAS did not converge", key)
}

func (s *SnapshotStore) Get(ctx context.Context, key string) (model.Envelope, error) {
	entry, err := s.kv.Get(ctx, key)
	if err != nil {
		return model.Envelope{}, err
	}
	var env model.Envelope
	if err := json.Unmarshal(entry.Value(), &env); err != nil {
		return model.Envelope{}, err
	}
	return env, env.Validate()
}

func (s *SnapshotStore) List(ctx context.Context, prefix string) ([]model.Envelope, error) {
	keys, err := s.kv.Keys(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.Envelope
	for _, key := range keys {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		env, err := s.Get(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("read snapshot %q: %w", key, err)
		}
		out = append(out, env)
	}
	return out, nil
}

func validDurable(s string) bool {
	if s == "" {
		return false
	}
	return !strings.ContainsAny(s, " .*\\/>\t\r\n")
}
func validSnapshotKey(s string) bool { return s != "" && !strings.ContainsAny(s, " *>\t\r\n/") }
func defaultDuration(v, d time.Duration) time.Duration {
	if v <= 0 {
		return d
	}
	return v
}
func defaultInt(v, d int) int {
	if v <= 0 {
		return d
	}
	return v
}
