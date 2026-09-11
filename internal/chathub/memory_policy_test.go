package chathub

import "testing"

// Upstream account memory must be off unless the operator opts in, and the
// decision must not depend on each call site remembering to set the flag.
//
// Regression for the 2026-09-11 audit: memory is a dial-time URL parameter with
// no payload equivalent, and only the answer turn set DisableMemory. The retry,
// repair, image and streaming turns built Request literals without it, so each
// of those turns ran with account memory enabled — and, once the connection pool
// reused their connections, so did unrelated requests.
func TestDisableMemoryForHonoursClientPolicy(t *testing.T) {
	// No policy installed: the request field is authoritative.
	bare := &Client{}
	if bare.disableMemoryFor(Request{}) {
		t.Fatal("with no policy the zero-value request must keep memory enabled")
	}
	if !bare.disableMemoryFor(Request{DisableMemory: true}) {
		t.Fatal("an explicit DisableMemory must be honoured")
	}

	// Policy on: every request disables memory, whether or not it asked.
	strict := &Client{MemoryPolicy: func() bool { return true }}
	if !strict.disableMemoryFor(Request{}) {
		t.Fatal("client policy must disable memory for a request that omitted the flag")
	}
	if !strict.disableMemoryFor(Request{DisableMemory: true}) {
		t.Fatal("client policy must stay disabled when the request also asked for it")
	}

	// Policy off: the request field is authoritative again.
	open := &Client{MemoryPolicy: func() bool { return false }}
	if open.disableMemoryFor(Request{}) {
		t.Fatal("policy=false must leave the request field in charge")
	}
	if !open.disableMemoryFor(Request{DisableMemory: true}) {
		t.Fatal("policy=false must still honour an explicit DisableMemory")
	}
}

// The resolved flag must reach the dial URL: that URL is the only place upstream
// reads memory behaviour from.
func TestDisableMemoryForReachesDialURL(t *testing.T) {
	acc := Account{OID: "oid", TID: "tid", AccessToken: "token"}
	on, err := BuildWSURLWithOptions(acc, "s", "c", "r", "Starter", "OfficeWebIncludedCopilot", true)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(on, "disableMemory=1") {
		t.Fatalf("disableMemory=1 missing from %s", on)
	}
	off, err := BuildWSURLWithOptions(acc, "s", "c", "r", "Starter", "OfficeWebIncludedCopilot", false)
	if err != nil {
		t.Fatal(err)
	}
	if contains(off, "disableMemory") {
		t.Fatalf("disableMemory must be absent when memory is on: %s", off)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
