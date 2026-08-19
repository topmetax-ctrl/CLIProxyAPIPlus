package handlers

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestCanonicalizeRequestedModelPreservesInbound(t *testing.T) {
	const inbound = "claude-fable-5-dd-raw.cursor-grok-4.6-xhigh-fast"
	const canonical = "cursor-grok-4.6-xhigh-fast"

	gotInbound, gotCanonical, body := canonicalizeRequestedModel(inbound, []byte(`{"model":"`+inbound+`"}`))
	if gotInbound != inbound {
		t.Fatalf("inbound = %q, want %q", gotInbound, inbound)
	}
	if gotCanonical != canonical {
		t.Fatalf("canonical = %q, want %q", gotCanonical, canonical)
	}
	if got := gjson.GetBytes(body, "model").String(); got != canonical {
		t.Fatalf("body model = %q, want %q", got, canonical)
	}
}

func TestCanonicalizeRequestedModelLeavesCanonicalUnchanged(t *testing.T) {
	const model = "cursor-grok-4.6-xhigh-fast"
	body := []byte(`{"model":"` + model + `"}`)

	gotInbound, gotCanonical, gotBody := canonicalizeRequestedModel(model, body)
	if gotInbound != model || gotCanonical != model {
		t.Fatalf("inbound/canonical = %q/%q, want %q/%q", gotInbound, gotCanonical, model, model)
	}
	if string(gotBody) != string(body) {
		t.Fatalf("body = %s, want unchanged %s", gotBody, body)
	}
}
