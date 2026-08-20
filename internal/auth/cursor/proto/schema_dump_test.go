package proto

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestAgentProtoDescriptorHasNoCacheUsageOnTurnEnded(t *testing.T) {
	fd := AgentFileDescriptor()

	tokenDelta := fd.Messages().ByName("TokenDeltaUpdate")
	if tokenDelta == nil {
		t.Fatal("TokenDeltaUpdate missing from embedded agent.proto")
	}
	if tokenDelta.Fields().Len() != 1 {
		t.Fatalf("TokenDeltaUpdate fields=%d, want 1", tokenDelta.Fields().Len())
	}
	f1 := tokenDelta.Fields().ByNumber(1)
	if f1 == nil || f1.Name() != "tokens" || f1.Kind() != protoreflect.Int32Kind {
		t.Fatalf("TokenDeltaUpdate field 1 = %v, want tokens int32", f1)
	}

	turnEnded := fd.Messages().ByName("TurnEndedUpdate")
	if turnEnded == nil {
		t.Fatal("TurnEndedUpdate missing from embedded agent.proto")
	}
	if turnEnded.Fields().Len() != 0 {
		t.Fatalf("descriptor TurnEndedUpdate fields=%d, want 0 (stale schema vs live wire)", turnEnded.Fields().Len())
	}

	usage := fd.Messages().ByName("TokenUsage")
	if usage != nil {
		t.Fatal("embedded descriptor unexpectedly defines TokenUsage")
	}
	if fd.Messages().ByName("Usage") != nil {
		t.Fatal("embedded descriptor unexpectedly defines Usage")
	}

	iu := fd.Messages().ByName("InteractionUpdate")
	if iu == nil {
		t.Fatal("InteractionUpdate missing")
	}
	if got := iu.Fields().ByName("token_delta"); got == nil || got.Number() != IU_TokenDelta {
		t.Fatalf("token_delta field = %v, want number %d", got, IU_TokenDelta)
	}
	if got := iu.Fields().ByName("turn_ended"); got == nil || got.Number() != IU_TurnEnded {
		t.Fatalf("turn_ended field = %v, want number %d", got, IU_TurnEnded)
	}
}

func TestGoldenTurnEndedHasFiveVarintsIgnoredByDescriptor(t *testing.T) {
	raw := readFixture(t, "parallel", "185-recv.bin")
	msg, err := DecodeAgentServerMessage(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if msg.Type != ServerMsgTurnEnded {
		t.Fatalf("type=%d, want TurnEnded", msg.Type)
	}
	got := TurnEndedVarints(msg.TurnEndedFields)
	want := map[int]int64{1: 24034, 2: 261, 3: 20014, 4: 0, 5: 0}
	if len(got) != len(want) {
		t.Fatalf("TurnEnded varints=%v hex=%s, want %v", got, hex.EncodeToString(msg.TurnEndedRaw), want)
	}
	for num, val := range want {
		if got[num] != val {
			t.Fatalf("field %d = %d, want %d (hex=%s)", num, got[num], val, hex.EncodeToString(msg.TurnEndedRaw))
		}
	}
	if hex.EncodeToString(msg.TurnEndedRaw) != "08e2bb0110850218ae9c0120002800" {
		t.Fatalf("TurnEnded raw hex=%s", hex.EncodeToString(msg.TurnEndedRaw))
	}
}

func TestGoldenTokenDeltaOnlyFieldOne(t *testing.T) {
	raw := readFixture(t, "parallel", "016-recv.bin")
	msg, err := DecodeAgentServerMessage(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if msg.Type != ServerMsgTokenDelta || msg.TokenDelta != 5 {
		t.Fatalf("type=%d tokens=%d, want TokenDelta 5", msg.Type, msg.TokenDelta)
	}
	payload := fieldBytes(raw, ASM_InteractionUpdate)
	tokenDelta := fieldBytes(payload, IU_TokenDelta)
	fields := InspectProtoFields(tokenDelta)
	if len(fields) != 1 || fields[0].Number != 1 || fields[0].Varint == nil || *fields[0].Varint != 5 {
		t.Fatalf("TokenDelta fields=%v, want [{1 varint 5}]", fields)
	}
}

func TestMaybeDumpWireNoopWithoutEnv(t *testing.T) {
	t.Setenv("CURSOR_WIRE_DUMP_DIR", "")
	MaybeDumpWire("turn_ended", nil, []byte{0x08, 0x01}, InspectProtoFields([]byte{0x08, 0x01}))
}

func TestMaybeDumpWireWritesSanitizedFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CURSOR_WIRE_DUMP_DIR", dir)
	raw := []byte{0x08, 0xe2, 0xbb, 0x01, 0x10, 0x85, 0x02}
	MaybeDumpWire("turn_ended", map[string]string{
		"conversation_id": "abcd1234",
		"model":           "cursor-grok-4.6-xhigh-fast",
	}, raw, InspectProtoFields(raw))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 {
		t.Fatalf("dump files=%d, want json+hex", len(entries))
	}
	foundJSON, foundHex := false, false
	for _, e := range entries {
		switch filepath.Ext(e.Name()) {
		case ".json":
			foundJSON = true
		case ".hex":
			foundHex = true
		}
	}
	if !foundJSON || !foundHex {
		t.Fatalf("missing dump files: %v", entries)
	}
}
