package proto

// Golden protocol fixture tests.
//
// The fixtures under testdata/cursor-2026-08/ are real sanitized wire frames
// captured from api2.cursor.sh AgentService/Run on 2026-08-13 with
// cmd/cursorcapture (model composer-2.5, x-cursor-client-version
// cli-2026.02.13-41ac335). They pin the decode/encode behavior of this
// package against the live protocol. When Cursor ships a protocol change,
// re-capture with cmd/cursorcapture, diff against these fixtures, and update
// deliberately.
//
// Key wire facts these fixtures prove (load-bearing for the executor):
//   - The server emits MULTIPLE ExecMcpArgs in one turn BEFORE any tool
//     result is sent (frames 064, 069, 075 arrive within ~140ms).
//   - After the mcpArgs batch the server writes blobs and then a
//     ConversationCheckpoint (frame 084), then goes quiet until results.
//   - Sending all McpResults on the same stream resumes the turn and it
//     finishes with TurnEnded (frame 185).

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

func readFixture(t *testing.T, dir, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "cursor-2026-08", dir, name))
	if err != nil {
		t.Fatalf("read fixture %s/%s: %v", dir, name, err)
	}
	return b
}

func decodeFixture(t *testing.T, name string) *DecodedServerMessage {
	t.Helper()
	msg, err := DecodeAgentServerMessage(readFixture(t, "parallel", name))
	if err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return msg
}

func TestGoldenDecodeTextAndThinking(t *testing.T) {
	if msg := decodeFixture(t, "015-recv.bin"); msg.Type != ServerMsgThinkingDelta || msg.Text == "" {
		t.Fatalf("015: type=%d text=%q, want ThinkingDelta with text", msg.Type, msg.Text)
	}
	if msg := decodeFixture(t, "021-recv.bin"); msg.Type != ServerMsgThinkingCompleted {
		t.Fatalf("021: type=%d, want ThinkingCompleted", msg.Type)
	}
	if msg := decodeFixture(t, "022-recv.bin"); msg.Type != ServerMsgTextDelta || msg.Text != "Đ" {
		t.Fatalf("022: type=%d text=%q, want TextDelta %q", msg.Type, msg.Text, "Đ")
	}
	if msg := decodeFixture(t, "016-recv.bin"); msg.Type != ServerMsgTokenDelta || msg.TokenDelta != 5 {
		t.Fatalf("016: type=%d tokens=%d, want TokenDelta 5", msg.Type, msg.TokenDelta)
	}
}

func TestGoldenDecodeKvMessages(t *testing.T) {
	get := decodeFixture(t, "003-recv.bin")
	if get.Type != ServerMsgKvGetBlob || len(get.BlobId) == 0 {
		t.Fatalf("003: type=%d blobId=%d bytes, want KvGetBlob with blob id", get.Type, len(get.BlobId))
	}
	set := decodeFixture(t, "007-recv.bin")
	if set.Type != ServerMsgKvSetBlob || len(set.BlobData) != 2184 {
		t.Fatalf("007: type=%d dataLen=%d, want KvSetBlob 2184 bytes", set.Type, len(set.BlobData))
	}
}

func TestGoldenDecodeExecRequestCtx(t *testing.T) {
	// The server's request-context frame carries its exec id nested inside the
	// request-context args submessage, not at the ExecServerMessage top level,
	// so msg.ExecId is legitimately empty here — the live server accepted our
	// EncodeExecRequestContextResult reply with an empty exec id (frame 006).
	msg := decodeFixture(t, "005-recv.bin")
	if msg.Type != ServerMsgExecRequestCtx {
		t.Fatalf("005: type=%d, want ExecRequestCtx", msg.Type)
	}
}

// TestGoldenDecodeParallelMcpArgs pins the core parallel-tools wire fact:
// three distinct ExecMcpArgs frames, each with its own execMsgId/execId,
// all captured before any tool result was sent.
func TestGoldenDecodeParallelMcpArgs(t *testing.T) {
	want := []struct {
		file      string
		tool      string
		execMsgId uint32
		argKey    string
	}{
		{"064-recv.bin", "get_stock_price", 1, "symbol"},
		{"069-recv.bin", "get_weather", 2, "city"},
		{"075-recv.bin", "get_exchange_rate", 3, "base"},
	}
	seenExecIds := map[string]bool{}
	for _, w := range want {
		msg := decodeFixture(t, w.file)
		if msg.Type != ServerMsgExecMcpArgs {
			t.Fatalf("%s: type=%d, want ExecMcpArgs", w.file, msg.Type)
		}
		if msg.McpToolName != w.tool {
			t.Fatalf("%s: tool=%q, want %q", w.file, msg.McpToolName, w.tool)
		}
		if msg.ExecMsgId != w.execMsgId {
			t.Fatalf("%s: execMsgId=%d, want %d", w.file, msg.ExecMsgId, w.execMsgId)
		}
		if msg.ExecId == "" || seenExecIds[msg.ExecId] {
			t.Fatalf("%s: exec id %q empty or duplicated", w.file, msg.ExecId)
		}
		seenExecIds[msg.ExecId] = true
		if msg.McpToolCallId == "" {
			t.Fatalf("%s: missing tool call id", w.file)
		}
		raw, ok := msg.McpArgs[w.argKey]
		if !ok {
			t.Fatalf("%s: missing arg %q (got %v)", w.file, w.argKey, keysOf(msg.McpArgs))
		}
		if _, err := ProtobufValueBytesToJSON(raw); err != nil {
			t.Fatalf("%s: arg %q does not decode as protobuf Value: %v", w.file, w.argKey, err)
		}
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestGoldenDecodeCheckpointAndTurnEnded(t *testing.T) {
	cp := decodeFixture(t, "084-recv.bin")
	if cp.Type != ServerMsgCheckpoint || len(cp.CheckpointData) != 3165 {
		t.Fatalf("084: type=%d cpLen=%d, want Checkpoint 3165 bytes", cp.Type, len(cp.CheckpointData))
	}
	end := decodeFixture(t, "185-recv.bin")
	if end.Type != ServerMsgTurnEnded {
		t.Fatalf("185: type=%d, want TurnEnded", end.Type)
	}
}

// TestGoldenDecodeUnknownInteractionUpdates verifies unknown/ignored
// interaction updates (tool_call_started/completed, step markers) decode
// without error and never masquerade as meaningful message types.
func TestGoldenDecodeUnknownInteractionUpdates(t *testing.T) {
	for _, file := range []string{"059-recv.bin", "062-recv.bin", "089-recv.bin", "183-recv.bin"} {
		msg := decodeFixture(t, file)
		switch msg.Type {
		case ServerMsgUnknown:
			// expected: gracefully ignored
		default:
			t.Fatalf("%s: unknown interaction update decoded as type=%d, want ServerMsgUnknown", file, msg.Type)
		}
	}
}

// TestGoldenEncodeMatchesRecordedFrames byte-compares deterministic encoder
// output against the exact frames that the live server accepted.
func TestGoldenEncodeMatchesRecordedFrames(t *testing.T) {
	// KvGetBlobResult for the system-prompt blob (frame 004 answered 003).
	get := decodeFixture(t, "003-recv.bin")
	sysBlob := mustSystemBlob(t)
	if got, want := EncodeKvGetBlobResult(get.KvId, sysBlob), readFixture(t, "parallel", "004-send.bin"); string(got) != string(want) {
		t.Fatalf("EncodeKvGetBlobResult drifted from recorded frame 004 (%d vs %d bytes)", len(got), len(want))
	}

	// KvSetBlobResult (frame 008 answered 007).
	set := decodeFixture(t, "007-recv.bin")
	if got, want := EncodeKvSetBlobResult(set.KvId), readFixture(t, "parallel", "008-send.bin"); string(got) != string(want) {
		t.Fatalf("EncodeKvSetBlobResult drifted from recorded frame 008")
	}

	// McpResult frames 086-088 answered execs 1-3 on the live stream.
	// dynamicpb.Marshal does not guarantee stable field ordering, so the
	// recorded frames and a fresh encode can differ byte-for-byte while being
	// semantically identical (the server accepted them). Compare by decoding
	// both back to their exec id + result content instead of raw bytes.
	for _, w := range []struct {
		reqFile, respFile, content string
	}{
		{"064-recv.bin", "086-send.bin", `{"symbol":"AAPL","price":231.45,"currency":"USD"}`},
		{"069-recv.bin", "087-send.bin", `{"city":"Tokyo","condition":"sunny","temp_c":27}`},
		{"075-recv.bin", "088-send.bin", `{"base":"USD","quote":"EUR","rate":0.9182}`},
	} {
		req := decodeFixture(t, w.reqFile)
		got := EncodeExecMcpResult(req.ExecMsgId, req.ExecId, w.content, false)
		want := readFixture(t, "parallel", w.respFile)
		gotID, gotContent := decodeExecMcpResult(t, got)
		wantID, wantContent := decodeExecMcpResult(t, want)
		if gotID != wantID || gotID != req.ExecId {
			t.Fatalf("EncodeExecMcpResult(%s): exec id got=%q recorded=%q want=%q", w.reqFile, gotID, wantID, req.ExecId)
		}
		if gotContent != wantContent || gotContent != w.content {
			t.Fatalf("EncodeExecMcpResult(%s): content got=%q recorded=%q want=%q", w.reqFile, gotContent, wantContent, w.content)
		}
	}
}

// decodeExecMcpResult pulls the exec_id and the MCP text-content string out of
// an AgentClientMessage(exec_client_message(mcp_result(success))) frame,
// walking the wire format directly so the assertion is independent of field
// ordering.
func decodeExecMcpResult(t *testing.T, data []byte) (execID, content string) {
	t.Helper()
	// AgentClientMessage.exec_client_message
	ecm := fieldBytes(data, ACM_ExecClientMessage)
	if ecm == nil {
		t.Fatalf("no exec_client_message in frame")
	}
	execID = string(fieldBytes(ecm, ECM_ExecId))
	mcpResult := fieldBytes(ecm, ECM_McpResult)
	success := fieldBytes(mcpResult, MCR_Success)  // McpResult.success
	item := fieldBytes(success, MCS_Content)       // McpSuccess.content[0]
	text := fieldBytes(item, MTRCI_Text)           // item.text (McpTextContent)
	content = string(fieldBytes(text, MTC_Text))   // McpTextContent.text
	return execID, content
}

// fieldBytes returns the length-delimited value of the first occurrence of the
// given field number in a protobuf message, or nil if absent.
func fieldBytes(data []byte, target int) []byte {
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return nil
		}
		data = data[n:]
		if typ == protowire.BytesType {
			val, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return nil
			}
			data = data[n:]
			if int(num) == target {
				return val
			}
		} else {
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				return nil
			}
			data = data[n:]
		}
	}
	return nil
}

// mustSystemBlob reproduces the system-prompt blob stored by EncodeRunRequest
// for the capture run ("You are a helpful assistant.").
func mustSystemBlob(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]string{"role": "system", "content": "You are a helpful assistant."})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestGoldenConnectEndStreamError pins Connect error trailer parsing against
// a live invalid_argument response (dead model composer-2).
func TestGoldenConnectEndStreamError(t *testing.T) {
	raw := readFixture(t, "error", "003-recv.bin")
	err := ParseConnectEndStream(raw)
	var ce *ConnectError
	if !errors.As(err, &ce) {
		t.Fatalf("expected ConnectError, got %v", err)
	}
	if ce.Code != "invalid_argument" {
		t.Fatalf("code=%q, want invalid_argument", ce.Code)
	}
}
