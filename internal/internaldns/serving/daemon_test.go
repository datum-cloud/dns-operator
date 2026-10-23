package serving

import "testing"

func TestCommandsNameOnlyKnownDaemons(t *testing.T) {
	for _, tc := range []struct {
		name    string
		set     func(*TransactionConfig, Daemon)
		daemon  Daemon
		invalid bool
	}{
		{name: "reload coredns", set: setReloadDaemon, daemon: DaemonCoreDNS},
		{name: "reload dnsdist", set: setReloadDaemon, daemon: DaemonDNSDist},
		{name: "reload unnamed", set: setReloadDaemon},
		{name: "reload unknown", set: setReloadDaemon, daemon: "bind", invalid: true},
		{name: "validate unknown", set: setValidateDaemon, daemon: "bind", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := regionalTestConfig(t)
			tc.set(&cfg.Transaction, tc.daemon)
			if err := cfg.Validate(); (err != nil) != tc.invalid {
				t.Fatalf("Validate() = %v, want invalid %v", err, tc.invalid)
			}
		})
	}
}

func setReloadDaemon(c *TransactionConfig, d Daemon) {
	c.Reload = []Command{{Daemon: d, Path: "reload"}}
}

func setValidateDaemon(c *TransactionConfig, d Daemon) {
	c.Validate = []Command{{Daemon: d, Path: "validate"}}
}
