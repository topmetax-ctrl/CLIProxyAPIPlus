package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cursorproto "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor/proto"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestPlanCursorContinuationModes(t *testing.T) {
	useCP, flatten, cont := planCursorContinuation("auto", false, true, true, true)
	if !useCP || flatten || cont != "checkpoint" {
		t.Fatalf("auto warm: %t %t %s", useCP, flatten, cont)
	}
	useCP, flatten, cont = planCursorContinuation("cold", false, true, true, true)
	if useCP || !flatten || cont != "flatten" {
		t.Fatalf("force cold: %t %t %s", useCP, flatten, cont)
	}
	useCP, flatten, cont = planCursorContinuation("auto", true, true, true, true)
	if useCP || !flatten || cont != "cold_continuation" {
		t.Fatalf("auto tool: %t %t %s", useCP, flatten, cont)
	}
	useCP, flatten, cont = planCursorContinuation("warm", true, true, true, true)
	if !useCP || flatten || cont != "checkpoint" {
		t.Fatalf("force warm tool: %t %t %s", useCP, flatten, cont)
	}
}

func TestContinuationModeRequiresSeam(t *testing.T) {
	t.Setenv(cursorContinuationSeamEnv, "")
	t.Setenv(cursorContinuationModeEnv, "cold")
	if got := continuationModeFromOptions(cliproxyexecutor.Options{}); got != "auto" {
		t.Fatalf("seam off: %s", got)
	}
	t.Setenv(cursorContinuationSeamEnv, "1")
	if got := continuationModeFromOptions(cliproxyexecutor.Options{}); got != "cold" {
		t.Fatalf("env mode: %s", got)
	}
}

func TestClassifyCursorTerminalStates(t *testing.T) {
	class, reason, expected := classifyCursorTerminal(nil, "tool_calls", nil)
	if class != cursorClassNoTurnEndedExpected || expected || reason != "tool_call_boundary_waiting_for_resume" {
		t.Fatalf("tool boundary: %s %s %t", class, reason, expected)
	}
	settled := &cursorTokenUsage{}
	settled.settleTurnEnded(liveCacheHitUsage())
	class, _, expected = classifyCursorTerminal(settled, "stop", nil)
	if class != cursorClassTurnEndedMatched || !expected {
		t.Fatalf("settled: %s %t", class, expected)
	}
	class, reason, expected = classifyCursorTerminal(&cursorTokenUsage{}, "eof", nil)
	if class != cursorClassTerminalUnavailable || !expected || reason != "eof_without_turn_ended" {
		t.Fatalf("eof: %s %s %t", class, reason, expected)
	}
	class, _, expected = classifyCursorTerminal(&cursorTokenUsage{}, "cancel", context.Canceled)
	if class != cursorClassTerminalUnavailable || expected {
		t.Fatalf("cancel: %s %t", class, expected)
	}
	class, reason, expected = classifyCursorTerminal(settled, "cancel", context.Canceled)
	if class != cursorClassTurnEndedMatched || !expected || reason != "turn_ended_settled" {
		t.Fatalf("TurnEnded must win over cancel: %s %s %t", class, reason, expected)
	}
}

func TestDumpCursorUsageSettledCorrelatesAuditID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CURSOR_WIRE_DUMP_DIR", dir)
	u := &cursorTokenUsage{}
	u.bindAudit(cursorAuditMeta{AuditID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", ConversationID: "conv", SessionID: "sess", Model: "m", Continuity: "checkpoint"})
	u.settleTurnEnded(liveCacheHitUsage())
	publishCursorUsageSettlement(u, "stop", nil)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var found map[string]any
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), "-usage_settled.json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &found); err != nil {
			t.Fatal(err)
		}
	}
	if found["audit_id"] != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Fatalf("audit_id=%v", found["audit_id"])
	}
	if found["classification"] != cursorClassTurnEndedMatched {
		t.Fatalf("class=%v", found["classification"])
	}
	if found["usage_source"] != "cursor_turn_ended" {
		t.Fatalf("source=%v", found["usage_source"])
	}
	if found["cache_read_tokens"] != float64(11392) && found["cache_read_tokens"] != int64(11392) {
		t.Fatalf("cache_read=%v", found["cache_read_tokens"])
	}
}

func TestProcessH2EOFWithoutTurnEndedDoesNotSettle(t *testing.T) {
	stream := newFakeCursorStream()
	usage := &cursorTokenUsage{}
	usage.setInputEstimate(40)
	errCh := make(chan error, 1)
	go func() {
		errCh <- processH2SessionFrames(context.Background(), stream, map[string][]byte{}, nil, nil, nil, nil, usage, nil)
	}()
	close(stream.data)
	if err := <-errCh; err != nil {
		t.Fatalf("eof: %v", err)
	}
	if usage.hasTerminal() {
		t.Fatal("EOF must not invent TurnEnded")
	}
	in, _ := usage.get()
	if in != 10 {
		t.Fatalf("heuristic input=%d", in)
	}
	class, reason, _ := classifyCursorTerminal(usage, "eof", nil)
	if class != cursorClassTerminalUnavailable || reason != "eof_without_turn_ended" {
		t.Fatalf("class=%s reason=%s", class, reason)
	}
}

func TestDecodeTurnEndedUnknownLengthDelimitedIsPreserved(t *testing.T) {
	raw := mustDecodeHex("08e259101618805920002812320568656c6c6f") // field 6 LEN "hello"
	u := cursorproto.DecodeTurnEndedUsage(raw, cursorproto.InspectProtoFields(raw))
	if !u.HasCacheRead || u.CacheReadTokens != 11392 {
		t.Fatalf("known fields lost: %+v", u)
	}
	if _, ok := u.Unknown[6]; !ok {
		t.Fatalf("length-delimited field 6 must be preserved: %+v", u)
	}
}
