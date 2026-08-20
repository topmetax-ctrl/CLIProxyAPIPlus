package proto

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

func readCursorUsageFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "cursor", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return b
}

func decodeTurnEndedFixture(t *testing.T, name string) *DecodedServerMessage {
	t.Helper()
	msg, err := DecodeAgentServerMessage(readCursorUsageFixture(t, name))
	if err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	if msg.Type != ServerMsgTurnEnded {
		t.Fatalf("%s type=%d, want TurnEnded", name, msg.Type)
	}
	return msg
}

func TestDecodeTurnEndedCacheHitExactFields(t *testing.T) {
	msg := decodeTurnEndedFixture(t, "turn_ended_cache_hit.bin")
	u := msg.TurnEndedUsage
	if !u.HasInput || u.InputTokens != 11490 {
		t.Fatalf("input=%d present=%t, want 11490", u.InputTokens, u.HasInput)
	}
	if !u.HasOutput || u.OutputTokens != 22 {
		t.Fatalf("output=%d present=%t, want 22", u.OutputTokens, u.HasOutput)
	}
	if !u.HasCacheRead || u.CacheReadTokens != 11392 {
		t.Fatalf("cache_read=%d present=%t, want 11392", u.CacheReadTokens, u.HasCacheRead)
	}
	if !u.HasCacheWrite || u.CacheWriteTokens != 0 {
		t.Fatalf("cache_write=%d present=%t, want 0 present", u.CacheWriteTokens, u.HasCacheWrite)
	}
	if !u.HasReasoning || u.ReasoningTokens != 18 {
		t.Fatalf("reasoning=%d present=%t, want 18", u.ReasoningTokens, u.HasReasoning)
	}
	if len(u.Unknown) != 0 {
		t.Fatalf("unknown=%v, want empty", u.Unknown)
	}
	if hex.EncodeToString(msg.TurnEndedRaw) != "08e259101618805920002812" {
		t.Fatalf("raw hex=%s", hex.EncodeToString(msg.TurnEndedRaw))
	}
}

func TestDecodeTurnEndedNoCacheStillPresent(t *testing.T) {
	u := decodeTurnEndedFixture(t, "turn_ended_no_cache.bin").TurnEndedUsage
	if !u.HasCacheRead || u.CacheReadTokens != 0 {
		t.Fatalf("cache_read=%d present=%t, want present 0", u.CacheReadTokens, u.HasCacheRead)
	}
	if !u.HasInput || u.InputTokens != 11490 {
		t.Fatalf("input=%d", u.InputTokens)
	}
}

func TestDecodeTurnEndedUnknownFieldsPreserved(t *testing.T) {
	u := decodeTurnEndedFixture(t, "turn_ended_unknown_fields.bin").TurnEndedUsage
	if u.CacheReadTokens != 11392 {
		t.Fatalf("cache_read=%d, want 11392", u.CacheReadTokens)
	}
	if _, ok := u.Unknown[6]; !ok {
		t.Fatalf("unknown fields=%v, want field 6", u.Unknown)
	}
}

func TestDecodeTurnEndedFieldOrderDoesNotChangeValues(t *testing.T) {
	u := decodeTurnEndedFixture(t, "turn_ended_reordered.bin").TurnEndedUsage
	if u.InputTokens != 11490 || u.OutputTokens != 22 || u.CacheReadTokens != 11392 || u.CacheWriteTokens != 0 || u.ReasoningTokens != 18 {
		t.Fatalf("reordered usage=%+v", u)
	}
}

func TestDecodeTurnEndedDuplicateFieldLastWins(t *testing.T) {
	u := decodeTurnEndedFixture(t, "turn_ended_duplicate_field3.bin").TurnEndedUsage
	if u.CacheReadTokens != 11392 {
		t.Fatalf("duplicate field 3 = %d, want last value 11392", u.CacheReadTokens)
	}
}

func TestDecodeTurnEndedLargeVarints(t *testing.T) {
	u := decodeTurnEndedFixture(t, "turn_ended_large_varints.bin").TurnEndedUsage
	if u.InputTokens != 1<<40 {
		t.Fatalf("input=%d, want %d", u.InputTokens, 1<<40)
	}
	if u.CacheReadTokens != 1<<35 {
		t.Fatalf("cache_read=%d, want %d", u.CacheReadTokens, 1<<35)
	}
}

func TestDecodeTurnEndedMalformedDoesNotInventFields(t *testing.T) {
	msg, err := DecodeAgentServerMessage(readCursorUsageFixture(t, "turn_ended_malformed.bin"))
	if err != nil {
		t.Fatalf("outer decode error=%v", err)
	}
	if msg.Type != ServerMsgTurnEnded {
		t.Fatalf("type=%d, want TurnEnded envelope", msg.Type)
	}
	if msg.TurnEndedUsage.HasInput || msg.TurnEndedUsage.HasCacheRead {
		t.Fatalf("malformed inner must not invent fields: %+v", msg.TurnEndedUsage)
	}
}

func TestDecodeTurnEndedEmptyMessageHasNoFields(t *testing.T) {
	inner := []byte{}
	iu := protowire.AppendTag(nil, IU_TurnEnded, protowire.BytesType)
	iu = protowire.AppendBytes(iu, inner)
	asm := protowire.AppendTag(nil, ASM_InteractionUpdate, protowire.BytesType)
	asm = protowire.AppendBytes(asm, iu)
	msg, err := DecodeAgentServerMessage(asm)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Type != ServerMsgTurnEnded || msg.TurnEndedUsage.HasAny() {
		t.Fatalf("empty TurnEnded should have no fields: %+v", msg.TurnEndedUsage)
	}
}

func TestDecodeTurnEndedUnknownLengthDelimitedDoesNotFail(t *testing.T) {
	inner := protowire.AppendTag(nil, 1, protowire.VarintType)
	inner = protowire.AppendVarint(inner, 10)
	inner = protowire.AppendTag(inner, 6, protowire.BytesType)
	inner = protowire.AppendBytes(inner, []byte("hello"))
	iu := protowire.AppendTag(nil, IU_TurnEnded, protowire.BytesType)
	iu = protowire.AppendBytes(iu, inner)
	asm := protowire.AppendTag(nil, ASM_InteractionUpdate, protowire.BytesType)
	asm = protowire.AppendBytes(asm, iu)
	msg, err := DecodeAgentServerMessage(asm)
	if err != nil {
		t.Fatal(err)
	}
	if !msg.TurnEndedUsage.HasInput || msg.TurnEndedUsage.InputTokens != 10 {
		t.Fatalf("known field lost: %+v", msg.TurnEndedUsage)
	}
	if _, ok := msg.TurnEndedUsage.Unknown[6]; !ok {
		t.Fatalf("unknown length-delimited field 6 missing: %+v", msg.TurnEndedUsage)
	}
}

func TestGoldenComposerTurnEndedMapsNamedFields(t *testing.T) {
	msg, err := DecodeAgentServerMessage(readFixture(t, "parallel", "185-recv.bin"))
	if err != nil {
		t.Fatal(err)
	}
	u := msg.TurnEndedUsage
	if u.InputTokens != 24034 || u.OutputTokens != 261 || u.CacheReadTokens != 20014 || u.CacheWriteTokens != 0 || u.ReasoningTokens != 0 {
		t.Fatalf("golden named usage=%+v", u)
	}
}
