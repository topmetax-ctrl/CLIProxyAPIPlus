// cursorcapture is a wire-level capture harness for the Cursor AgentService
// protocol. It answers the load-bearing design question for parallel tool
// support: does the upstream emit multiple mcpArgs exec requests in one turn
// BEFORE receiving any tool result, or does it serialize them one-by-one?
//
// Unlike the production executor, this tool never returns after the first
// mcpArgs. It keeps draining frames, records every send/recv frame to disk
// (raw bytes + JSONL index) for golden fixtures, and only after a quiet
// window sends ALL collected tool results back on the same stream, then
// keeps reading until TurnEnded.
//
// Usage:
//
//	go run ./cmd/cursorcapture -auth ~/.cli-proxy-api/cursor.XXXX.json \
//	  -model composer-2.5 -out capture/parallel-01
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	cursorproto "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor/proto"
	log "github.com/sirupsen/logrus"
)

const clientVersion = "cli-2026.02.13-41ac335"

type authFile struct {
	AccessToken string `json:"access_token"`
}

type indexEntry struct {
	Seq     int    `json:"seq"`
	TsMs    int64  `json:"ts_ms"`
	Dir     string `json:"dir"` // send | recv
	Flags   byte   `json:"flags"`
	Len     int    `json:"len"`
	Kind    string `json:"kind"`
	Detail  string `json:"detail,omitempty"`
	RawFile string `json:"raw_file"`
}

type recorder struct {
	mu    sync.Mutex
	dir   string
	seq   int
	start time.Time
	idx   *os.File
}

func newRecorder(dir string) (*recorder, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.Create(filepath.Join(dir, "index.jsonl"))
	if err != nil {
		return nil, err
	}
	return &recorder{dir: dir, start: time.Now(), idx: f}, nil
}

func (r *recorder) record(dir string, flags byte, payload []byte, kind, detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	name := fmt.Sprintf("%03d-%s.bin", r.seq, dir)
	_ = os.WriteFile(filepath.Join(r.dir, name), payload, 0o644)
	e := indexEntry{
		Seq: r.seq, TsMs: time.Since(r.start).Milliseconds(), Dir: dir,
		Flags: flags, Len: len(payload), Kind: kind, Detail: detail, RawFile: name,
	}
	b, _ := json.Marshal(e)
	fmt.Fprintln(r.idx, string(b))
	log.Infof("[%6dms] #%03d %s %-22s flags=0x%02x len=%d %s", e.TsMs, e.Seq, dir, kind, flags, len(payload), detail)
}

func (r *recorder) close() { r.idx.Close() }

type pendingExec struct {
	ExecMsgId  uint32
	ExecId     string
	ToolCallId string
	ToolName   string
	ArgsJSON   string
	AtMs       int64
}

func decodeArgs(args map[string][]byte) string {
	out := make(map[string]any, len(args))
	for k, v := range args {
		if dv, err := cursorproto.ProtobufValueBytesToJSON(v); err == nil {
			out[k] = dv
		} else {
			out[k] = string(v)
		}
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func fakeResult(toolName string) string {
	switch toolName {
	case "get_stock_price":
		return `{"symbol":"AAPL","price":231.45,"currency":"USD"}`
	case "get_weather":
		return `{"city":"Tokyo","condition":"sunny","temp_c":27}`
	case "get_exchange_rate":
		return `{"base":"USD","quote":"EUR","rate":0.9182}`
	default:
		return `{"ok":true}`
	}
}

func main() {
	authPath := flag.String("auth", "", "path to cli-proxy-api cursor auth json")
	model := flag.String("model", "composer-2.5", "model id")
	outDir := flag.String("out", "capture/run", "output directory for frames")
	quietSec := flag.Int("quiet", 8, "seconds after last mcpArgs before sending all results")
	maxSec := flag.Int("max", 150, "hard timeout in seconds")
	prompt := flag.String("prompt", "", "override user prompt")
	flag.Parse()

	log.SetLevel(log.InfoLevel)
	log.SetFormatter(&log.TextFormatter{DisableTimestamp: true})

	if *authPath == "" {
		log.Fatal("missing -auth")
	}
	raw, err := os.ReadFile(*authPath)
	if err != nil {
		log.Fatalf("read auth: %v", err)
	}
	var af authFile
	if err := json.Unmarshal(raw, &af); err != nil || af.AccessToken == "" {
		log.Fatalf("parse auth: %v", err)
	}

	rec, err := newRecorder(*outDir)
	if err != nil {
		log.Fatalf("recorder: %v", err)
	}
	defer rec.close()

	userText := *prompt
	if userText == "" {
		userText = "You have three independent tools. In your FIRST response you MUST call all three tools " +
			"simultaneously (in parallel, in the same turn, before receiving any result): " +
			"1) get_stock_price with symbol=AAPL, 2) get_weather with city=Tokyo, 3) get_exchange_rate with base=USD quote=EUR. " +
			"Do not call them one at a time. Issue all three tool calls at once, then wait. " +
			"After all three results arrive, summarize them in one short sentence."
	}

	tools := []cursorproto.McpToolDef{
		{Name: "get_stock_price", Description: "Get current stock price for a ticker symbol", InputSchema: json.RawMessage(`{"type":"object","properties":{"symbol":{"type":"string"}},"required":["symbol"]}`)},
		{Name: "get_weather", Description: "Get current weather for a city", InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`)},
		{Name: "get_exchange_rate", Description: "Get FX rate between two currencies", InputSchema: json.RawMessage(`{"type":"object","properties":{"base":{"type":"string"},"quote":{"type":"string"}},"required":["base","quote"]}`)},
	}

	params := &cursorproto.RunRequestParams{
		ModelId:        *model,
		SystemPrompt:   "You are a helpful assistant.",
		UserText:       userText,
		MessageId:      uuid.New().String(),
		ConversationId: uuid.New().String(),
		McpTools:       tools,
		BlobStore:      make(map[string][]byte),
	}

	reqBytes := cursorproto.EncodeRunRequest(params)
	rec.record("send", 0, reqBytes, "RunRequest", fmt.Sprintf("model=%s tools=%d", *model, len(tools)))

	headers := map[string]string{
		":path":                    "/agent.v1.AgentService/Run",
		"content-type":             "application/connect+proto",
		"connect-protocol-version": "1",
		"te":                       "trailers",
		"authorization":            "Bearer " + af.AccessToken,
		"x-ghost-mode":             "true",
		"x-cursor-client-version":  clientVersion,
		"x-cursor-client-type":     "cli",
		"x-request-id":             uuid.New().String(),
	}
	stream, err := cursorproto.DialH2Stream("api2.cursor.sh", headers)
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer stream.Close()

	send := func(payload []byte, kind, detail string) {
		rec.record("send", 0, payload, kind, detail)
		if err := stream.Write(cursorproto.FrameConnectMessage(payload, 0)); err != nil {
			log.Errorf("write %s: %v", kind, err)
		}
	}

	if err := stream.Write(cursorproto.FrameConnectMessage(reqBytes, 0)); err != nil {
		log.Fatalf("send run request: %v", err)
	}

	var closed atomic.Bool
	go func() { // heartbeat
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for range t.C {
			if closed.Load() {
				return
			}
			if err := stream.Write(cursorproto.FrameConnectMessage(cursorproto.EncodeHeartbeat(), 0)); err != nil {
				return
			}
		}
	}()

	var (
		buf           []byte
		pending       []pendingExec
		resultsSent   bool
		lastMcpArgsAt time.Time
		deadline      = time.After(time.Duration(*maxSec) * time.Second)
		quietTick     = time.NewTicker(500 * time.Millisecond)
	)
	defer quietTick.Stop()

	finish := func(reason string) {
		closed.Store(true)
		log.Infof("=== FINISH: %s ===", reason)
		log.Infof("mcpArgs collected: %d", len(pending))
		for i, p := range pending {
			log.Infof("  [%d] at=%dms tool=%s execMsgId=%d execId=%s callId=%s args=%s",
				i, p.AtMs, p.ToolName, p.ExecMsgId, p.ExecId, p.ToolCallId, p.ArgsJSON)
		}
		summary := map[string]any{
			"reason": reason, "mcp_args_count": len(pending), "results_sent": resultsSent,
			"pending": pending, "model": *model,
		}
		b, _ := json.MarshalIndent(summary, "", "  ")
		_ = os.WriteFile(filepath.Join(*outDir, "summary.json"), b, 0o644)
	}

	sendAllResults := func() {
		for _, p := range pending {
			payload := cursorproto.EncodeExecMcpResult(p.ExecMsgId, p.ExecId, fakeResult(p.ToolName), false)
			send(payload, "McpResult", fmt.Sprintf("tool=%s execMsgId=%d", p.ToolName, p.ExecMsgId))
		}
		resultsSent = true
	}

	for {
		select {
		case <-deadline:
			finish("hard timeout")
			return

		case <-quietTick.C:
			if !resultsSent && len(pending) > 0 && time.Since(lastMcpArgsAt) > time.Duration(*quietSec)*time.Second {
				log.Infof(">>> quiet window elapsed after %d mcpArgs — sending ALL results now", len(pending))
				sendAllResults()
			}

		case data, ok := <-stream.Data():
			if !ok {
				finish(fmt.Sprintf("stream closed, err=%v", stream.Err()))
				return
			}
			buf = append(buf, data...)
			for {
				flags, payload, consumed, ok := cursorproto.ParseConnectFrame(buf)
				if !ok {
					break
				}
				buf = buf[consumed:]

				if flags&cursorproto.ConnectEndStreamFlag != 0 {
					err := cursorproto.ParseConnectEndStream(payload)
					rec.record("recv", flags, payload, "EndStream", fmt.Sprintf("err=%v", err))
					continue
				}

				msg, derr := cursorproto.DecodeAgentServerMessage(payload)
				if derr != nil {
					rec.record("recv", flags, payload, "DecodeError", derr.Error())
					continue
				}

				switch msg.Type {
				case cursorproto.ServerMsgTextDelta:
					rec.record("recv", flags, payload, "TextDelta", fmt.Sprintf("%q", msg.Text))
				case cursorproto.ServerMsgThinkingDelta:
					rec.record("recv", flags, payload, "ThinkingDelta", fmt.Sprintf("%d chars", len(msg.Text)))
				case cursorproto.ServerMsgThinkingCompleted:
					rec.record("recv", flags, payload, "ThinkingCompleted", "")
				case cursorproto.ServerMsgHeartbeat:
					rec.record("recv", flags, payload, "Heartbeat", "")
				case cursorproto.ServerMsgTokenDelta:
					rec.record("recv", flags, payload, "TokenDelta", fmt.Sprintf("%d", msg.TokenDelta))
				case cursorproto.ServerMsgCheckpoint:
					rec.record("recv", flags, payload, "Checkpoint", fmt.Sprintf("%d bytes", len(msg.CheckpointData)))
				case cursorproto.ServerMsgTurnEnded:
					rec.record("recv", flags, payload, "TurnEnded", "")
					if resultsSent {
						finish("turn ended after results")
					} else {
						finish("turn ended BEFORE any results were sent")
					}
					return

				case cursorproto.ServerMsgKvGetBlob:
					key := cursorproto.BlobIdHex(msg.BlobId)
					rec.record("recv", flags, payload, "KvGetBlob", key[:16])
					send(cursorproto.EncodeKvGetBlobResult(msg.KvId, params.BlobStore[key]), "KvGetBlobResult", key[:16])
				case cursorproto.ServerMsgKvSetBlob:
					key := cursorproto.BlobIdHex(msg.BlobId)
					params.BlobStore[key] = append([]byte(nil), msg.BlobData...)
					rec.record("recv", flags, payload, "KvSetBlob", fmt.Sprintf("%s %d bytes", key[:16], len(msg.BlobData)))
					send(cursorproto.EncodeKvSetBlobResult(msg.KvId), "KvSetBlobResult", key[:16])
				case cursorproto.ServerMsgExecRequestCtx:
					rec.record("recv", flags, payload, "ExecRequestCtx", fmt.Sprintf("execMsgId=%d", msg.ExecMsgId))
					send(cursorproto.EncodeExecRequestContextResult(msg.ExecMsgId, msg.ExecId, params.McpTools), "RequestCtxResult", "")

				case cursorproto.ServerMsgExecMcpArgs:
					p := pendingExec{
						ExecMsgId: msg.ExecMsgId, ExecId: msg.ExecId,
						ToolCallId: msg.McpToolCallId, ToolName: msg.McpToolName,
						ArgsJSON: decodeArgs(msg.McpArgs), AtMs: time.Since(rec.start).Milliseconds(),
					}
					pending = append(pending, p)
					lastMcpArgsAt = time.Now()
					rec.record("recv", flags, payload, "ExecMcpArgs",
						fmt.Sprintf(">>> #%d tool=%s execMsgId=%d args=%s", len(pending), p.ToolName, p.ExecMsgId, p.ArgsJSON))

				default:
					rec.record("recv", flags, payload, fmt.Sprintf("Other(type=%d)", msg.Type), fmt.Sprintf("execField=%d", msg.ExecFieldNumber))
				}
			}

		case <-stream.Done():
			finish(fmt.Sprintf("stream done, err=%v", stream.Err()))
			return
		}
	}
}
