package main

import "testing"

// TestWebhookStartupGuardFailsClosedOnEmptySecret is the operational half of
// CIC-80: a production server with no webhook signing secret must refuse to
// start unless the operator has explicitly selected the development-only
// fail-open path.
func TestWebhookStartupGuardFailsClosedOnEmptySecret(t *testing.T) {
	cases := []struct {
		name     string
		secret   string
		insecure bool
		wantErr  bool
	}{
		{name: "secret configured", secret: "whsec_abc123", insecure: false, wantErr: false},
		{name: "no secret, no opt in (fail closed)", secret: "", insecure: false, wantErr: true},
		{name: "no secret, dev opt in", secret: "", insecure: true, wantErr: false},
		{name: "both set, insecure flag accepted", secret: "s", insecure: true, wantErr: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := webhookStartupGuard(c.secret, c.insecure)
			if (err != nil) != c.wantErr {
				t.Errorf("webhookStartupGuard(%q, %v) err=%v, wantErr=%v", c.secret, c.insecure, err, c.wantErr)
			}
		})
	}
}
