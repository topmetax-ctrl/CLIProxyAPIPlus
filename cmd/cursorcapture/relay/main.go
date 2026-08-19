// Command relay is an audit-only recording pass-through for the official Cursor
// CLI. Point `cursor-agent --endpoint http://127.0.0.1:<port>` at it and every
// request/response body is written to disk before being forwarded verbatim to
// the real upstream over TLS.
//
// It never rewrites, re-signs or otherwise alters a request: headers and body
// bytes are forwarded byte-for-byte so upstream authentication and anti-abuse
// checks behave exactly as they do without the relay. Only sanitized metadata is
// written to the transcript; credential-bearing headers are recorded as a
// presence flag plus length, never as a value.
package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

var secretHeaders = map[string]bool{
	"authorization":           true,
	"cookie":                  true,
	"x-cursor-checksum":       true,
	"x-cursor-client-key":     true,
	"x-amzn-trace-id":         true,
	"x-request-id":            true,
	"x-cursor-config-version": true,
}

type entry struct {
	Seq         int64             `json:"seq"`
	At          string            `json:"at"`
	Method      string            `json:"method"`
	Path        string            `json:"path"`
	ReqHeaders  map[string]string `json:"req_headers"`
	ReqBytes    int               `json:"req_bytes"`
	ReqFile     string            `json:"req_file,omitempty"`
	Status      int               `json:"status"`
	RespHeaders map[string]string `json:"resp_headers"`
	RespBytes   int               `json:"resp_bytes"`
	RespFile    string            `json:"resp_file,omitempty"`
	DurationMs  int64             `json:"duration_ms"`
	Error       string            `json:"error,omitempty"`
}

func sanitize(h http.Header) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		lk := strings.ToLower(k)
		if secretHeaders[lk] {
			out[lk] = fmt.Sprintf("<redacted len=%d>", len(strings.Join(v, "")))
			continue
		}
		out[lk] = strings.Join(v, ",")
	}
	return out
}

func main() {
	listen := flag.String("listen", "127.0.0.1:8500", "listen address (h2c + http/1.1)")
	upstream := flag.String("upstream", "https://api2.cursor.sh", "upstream base URL")
	outDir := flag.String("out", "capture/official-relay", "output directory")
	flag.Parse()

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "mkdir:", err)
		os.Exit(1)
	}
	idxFile, err := os.Create(filepath.Join(*outDir, "index.jsonl"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "index:", err)
		os.Exit(1)
	}
	defer func() {
		if errClose := idxFile.Close(); errClose != nil {
			fmt.Fprintln(os.Stderr, "close index:", errClose)
		}
	}()

	client := &http.Client{
		Transport: &http2.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		},
	}

	var seq int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&seq, 1)
		start := time.Now()
		e := entry{Seq: n, At: start.UTC().Format(time.RFC3339Nano), Method: r.Method,
			Path: r.URL.Path, ReqHeaders: sanitize(r.Header)}

		// Stream the request body through instead of buffering: AgentService/Run
		// is bidirectional, so buffering would deadlock the stream.
		e.ReqFile = fmt.Sprintf("%04d-req.bin", n)
		reqDump, errDump := os.Create(filepath.Join(*outDir, e.ReqFile))
		if errDump != nil {
			fmt.Fprintln(os.Stderr, "create req file:", errDump)
		}
		counter := &teeCounter{src: r.Body, dump: reqDump}

		target := strings.TrimRight(*upstream, "/") + r.URL.Path
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		outReq, errNew := http.NewRequestWithContext(r.Context(), r.Method, target, counter)
		if errNew != nil {
			e.Error = errNew.Error()
			writeEntry(idxFile, e)
			http.Error(w, errNew.Error(), http.StatusBadGateway)
			return
		}
		outReq.ContentLength = -1
		defer func() {
			e.ReqBytes = counter.total()
			if reqDump != nil {
				if errClose := reqDump.Close(); errClose != nil {
					fmt.Fprintln(os.Stderr, "close req file:", errClose)
				}
			}
		}()
		for k, v := range r.Header {
			for _, vv := range v {
				outReq.Header.Add(k, vv)
			}
		}
		outReq.Header.Del("Host")

		resp, errDo := client.Do(outReq)
		if errDo != nil {
			e.Error = errDo.Error()
			e.DurationMs = time.Since(start).Milliseconds()
			writeEntry(idxFile, e)
			http.Error(w, errDo.Error(), http.StatusBadGateway)
			return
		}
		defer func() {
			if errClose := resp.Body.Close(); errClose != nil {
				fmt.Fprintln(os.Stderr, "close upstream body:", errClose)
			}
		}()

		e.Status = resp.StatusCode
		e.RespHeaders = sanitize(resp.Header)
		for k, v := range resp.Header {
			for _, vv := range v {
				w.Header().Add(k, vv)
			}
		}
		w.WriteHeader(resp.StatusCode)

		respFile := fmt.Sprintf("%04d-resp.bin", n)
		f, errCreate := os.Create(filepath.Join(*outDir, respFile))
		if errCreate != nil {
			fmt.Fprintln(os.Stderr, "create resp file:", errCreate)
		}
		var total int
		buf := make([]byte, 32*1024)
		for {
			nr, errRead := resp.Body.Read(buf)
			if nr > 0 {
				total += nr
				if f != nil {
					_, _ = f.Write(buf[:nr])
				}
				if _, errW := w.Write(buf[:nr]); errW != nil {
					break
				}
				if fl, ok := w.(http.Flusher); ok {
					fl.Flush()
				}
			}
			if errRead != nil {
				break
			}
		}
		if f != nil {
			if errClose := f.Close(); errClose != nil {
				fmt.Fprintln(os.Stderr, "close resp file:", errClose)
			}
		}
		e.RespBytes = total
		e.RespFile = respFile
		e.DurationMs = time.Since(start).Milliseconds()
		writeEntry(idxFile, e)
		fmt.Fprintf(os.Stderr, "[relay] %d %s %s -> %d (%d req / %d resp bytes)\n",
			n, r.Method, r.URL.Path, resp.StatusCode, e.ReqBytes, total)
	})

	h2s := &http2.Server{}
	srv := &http.Server{Addr: *listen, Handler: h2c.NewHandler(handler, h2s)}
	fmt.Fprintf(os.Stderr, "[relay] listening on %s -> %s, out=%s\n", *listen, *upstream, *outDir)
	if errListen := srv.ListenAndServe(); errListen != nil {
		fmt.Fprintln(os.Stderr, "listen:", errListen)
		os.Exit(1)
	}
}

// teeCounter forwards a request body verbatim while mirroring the bytes to a
// dump file and counting them.
type teeCounter struct {
	src  io.ReadCloser
	dump *os.File
	n    int64
	mu   sync.Mutex
}

func (t *teeCounter) Read(p []byte) (int, error) {
	n, err := t.src.Read(p)
	if n > 0 {
		t.mu.Lock()
		t.n += int64(n)
		t.mu.Unlock()
		if t.dump != nil {
			_, _ = t.dump.Write(p[:n])
		}
	}
	return n, err
}

func (t *teeCounter) Close() error { return t.src.Close() }

func (t *teeCounter) total() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return int(t.n)
}

func writeEntry(f *os.File, e entry) {
	b, _ := json.Marshal(e)
	_, _ = f.Write(append(b, '\n'))
	_ = f.Sync()
}
