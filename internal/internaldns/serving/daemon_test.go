package serving

import "testing"

func TestCommandsNameOnlyKnownDaemons(t *testing.T) {
	cfg := regionalTestConfig(t)
	cfg.Transaction.Reload = []Command{{Daemon: DaemonCoreDNS, Path: "reload"}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.Transaction.Reload = []Command{{Daemon: "bind", Path: "reload"}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("unknown daemon passed validation")
	}
}
