package transport

import (
	"errors"
	"testing"
)

func TestCheckBaseURLAllowsWhatCannotLeave(t *testing.T) {
	for _, raw := range []string{
		"https://navarch.example.com",
		"https://10.0.1.7:8417", // TLS anywhere is fine, private or not
		"http://localhost:8417",
		"http://127.0.0.1:8417",
		"http://[::1]:8417",
		"http://127.2.3.4:8417", // all of 127/8 is loopback
		"http://pr-1.preview.localhost",
		"http://controlplane:8417", // the dev stack's agents
		"http://navarch.internal:8417",
		"http://LOCALHOST:8417", // case is not a bypass
		"http://localhost.:8417",
	} {
		if err := CheckBaseURL(raw); err != nil {
			t.Errorf("CheckBaseURL(%q) = %v, want nil", raw, err)
		}
	}
}

// The refusals that matter. A private address is the case the audit named — a
// shared network where a captured node token reads ciphertext for as long as it
// is valid — so it must not be waved through for looking internal.
func TestCheckBaseURLRefusesPlaintextThatCanLeave(t *testing.T) {
	for _, raw := range []string{
		"http://navarch.example.com",
		"http://10.0.1.7:8417",
		"http://192.168.1.10:8417",
		"http://172.16.4.4:8417",
		"http://203.0.113.10:8417",
		"http://node.internal.example.com", // .internal must be the suffix, not a label
	} {
		err := CheckBaseURL(raw)
		var ie *InsecureError
		if !errors.As(err, &ie) {
			t.Errorf("CheckBaseURL(%q) = %v, want an InsecureError", raw, err)
		}
	}
}

// Malformed input is not something the opt-in should rescue, so it must not
// come back as InsecureError — otherwise "I trust this network" would also mean
// "and I accept a URL nothing can parse".
func TestUnusableURLsAreNotMerelyInsecure(t *testing.T) {
	for _, raw := range []string{
		"localhost:8417", // no scheme: parses as scheme "localhost"
		"ftp://localhost:8417",
		"http://",
		"://nope",
	} {
		err := CheckBaseURL(raw)
		if err == nil {
			t.Errorf("CheckBaseURL(%q) = nil, want an error", raw)
			continue
		}
		var ie *InsecureError
		if errors.As(err, &ie) {
			t.Errorf("CheckBaseURL(%q) returned InsecureError; an opt-in must not rescue it", raw)
		}
	}
}

func TestInsecureOptIn(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on", " 1 "} {
		if !Insecure(v) {
			t.Errorf("Insecure(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "0", "false", "no", "maybe"} {
		if Insecure(v) {
			t.Errorf("Insecure(%q) = true, want false", v)
		}
	}
}
