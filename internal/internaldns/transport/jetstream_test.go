package transport

import "testing"

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
