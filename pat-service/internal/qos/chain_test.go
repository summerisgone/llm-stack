package qos

import (
	"encoding/json"
	"testing"
)

func mustChatRequest(t *testing.T, body string) chatRequest {
	t.Helper()
	req, ok := parseChatRequest([]byte(body))
	if !ok {
		t.Fatalf("parseChatRequest(%q) rejected a well-formed chat request", body)
	}
	return req
}

func TestParseChatRequestRejectsNonChatBodies(t *testing.T) {
	cases := []string{
		`{"model":"m","prompt":"hi"}`, // /v1/completions shape, no messages
		`{"model":"m","messages":[]}`, // empty messages
		`not json`,                    // malformed
		`{"model":"m"}`,               // missing messages entirely
	}
	for _, body := range cases {
		if _, ok := parseChatRequest([]byte(body)); ok {
			t.Errorf("parseChatRequest(%q) = ok, want rejected", body)
		}
	}
}

func TestChainIsStableUnderKeyReordering(t *testing.T) {
	a := mustChatRequest(t, `{"model":"m","tools":null,"messages":[{"role":"user","content":"hi"}]}`)
	b := mustChatRequest(t, `{"tools":null,"messages":[{"content":"hi","role":"user"}],"model":"m"}`)

	chainA, err := chain(a)
	if err != nil {
		t.Fatal(err)
	}
	chainB, err := chain(b)
	if err != nil {
		t.Fatal(err)
	}
	if chainA[0] != chainB[0] {
		t.Fatal("key order and object order changed the chain hash; canonicalize is not sorting deterministically")
	}
}

func TestChainDivergesOnDifferentFirstMessage(t *testing.T) {
	a := mustChatRequest(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	b := mustChatRequest(t, `{"model":"m","messages":[{"role":"user","content":"bye"}]}`)

	chainA, _ := chain(a)
	chainB, _ := chain(b)
	if chainA[0] == chainB[0] {
		t.Fatal("different first messages produced the same c_0")
	}
}

func TestChainExtendsPreviousLinkWhenPrefixShared(t *testing.T) {
	step1 := mustChatRequest(t, `{"model":"m","messages":[
		{"role":"user","content":"hi"}
	]}`)
	step2 := mustChatRequest(t, `{"model":"m","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"hello"},
		{"role":"user","content":"and then?"}
	]}`)

	c1, _ := chain(step1)
	c2, _ := chain(step2)

	if len(c2) != 3 {
		t.Fatalf("chain length = %d, want 3", len(c2))
	}
	if c1[0] != c2[0] {
		t.Fatal("shared first message did not produce the same c_0 across requests")
	}
}

func TestCanonicalizeSortsNestedObjectKeys(t *testing.T) {
	out, err := canonicalize(json.RawMessage(`{"b":1,"a":{"z":1,"y":2}}`))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":{"y":2,"z":1},"b":1}`
	if string(out) != want {
		t.Fatalf("canonicalize = %s, want %s", out, want)
	}
}
