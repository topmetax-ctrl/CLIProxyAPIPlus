package proto

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

// WireField is a sanitized protobuf field observation. Byte values are
// recorded as length + hex so dumps stay decodeable without carrying secrets
// from surrounding frames.
type WireField struct {
	Number   int         `json:"number"`
	WireType string      `json:"wire_type"`
	Varint   *int64      `json:"varint,omitempty"`
	BytesLen int         `json:"bytes_len,omitempty"`
	BytesHex string      `json:"bytes_hex,omitempty"`
	Nested   []WireField `json:"nested,omitempty"`
}

// InspectProtoFields walks a protobuf message and returns every field that
// decodes cleanly. Nested length-delimited values are inspected when they
// themselves look like protobuf. This is observation-only.
func InspectProtoFields(data []byte) []WireField {
	var out []WireField
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		field := WireField{Number: int(num), WireType: wireTypeName(typ)}
		switch typ {
		case protowire.VarintType:
			val, n := protowire.ConsumeVarint(data)
			if n < 0 {
				return out
			}
			data = data[n:]
			v := int64(val)
			field.Varint = &v
		case protowire.BytesType:
			val, n := protowire.ConsumeBytes(data)
			if n < 0 {
				return out
			}
			data = data[n:]
			field.BytesLen = len(val)
			field.BytesHex = hex.EncodeToString(val)
			if looksLikeProto(val) {
				field.Nested = InspectProtoFields(val)
			}
		case protowire.Fixed32Type:
			val, n := protowire.ConsumeFixed32(data)
			if n < 0 {
				return out
			}
			data = data[n:]
			v := int64(val)
			field.Varint = &v
		case protowire.Fixed64Type:
			val, n := protowire.ConsumeFixed64(data)
			if n < 0 {
				return out
			}
			data = data[n:]
			v := int64(val)
			field.Varint = &v
		default:
			n := protowire.ConsumeFieldValue(num, typ, data)
			if n < 0 {
				return out
			}
			data = data[n:]
		}
		out = append(out, field)
	}
	return out
}

// TurnEndedVarints returns field-number → varint for TurnEndedUpdate.
// A missing key means the field was absent, not zero.
func TurnEndedVarints(fields []WireField) map[int]int64 {
	out := make(map[int]int64, len(fields))
	for _, f := range fields {
		if f.Varint != nil && (f.WireType == "varint" || f.WireType == "fixed32" || f.WireType == "fixed64") {
			out[f.Number] = *f.Varint
		}
	}
	return out
}

func formatWireFields(fields []WireField) string {
	if len(fields) == 0 {
		return "[]"
	}
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		if f.Varint != nil {
			parts = append(parts, fmt.Sprintf("%d=%d", f.Number, *f.Varint))
			continue
		}
		if f.BytesLen > 0 {
			parts = append(parts, fmt.Sprintf("%d=bytes:%d", f.Number, f.BytesLen))
			continue
		}
		parts = append(parts, fmt.Sprintf("%d=%s", f.Number, f.WireType))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func wireTypeName(typ protowire.Type) string {
	switch typ {
	case protowire.VarintType:
		return "varint"
	case protowire.BytesType:
		return "bytes"
	case protowire.Fixed32Type:
		return "fixed32"
	case protowire.Fixed64Type:
		return "fixed64"
	case protowire.StartGroupType:
		return "group"
	default:
		return fmt.Sprintf("wire_%d", int(typ))
	}
}

func looksLikeProto(val []byte) bool {
	if len(val) == 0 {
		return false
	}
	num, typ, n := protowire.ConsumeTag(val)
	if n < 0 || num < 1 {
		return false
	}
	rest := protowire.ConsumeFieldValue(num, typ, val[n:])
	return rest >= 0
}

var wireDumpSeq atomic.Uint64

// MaybeDumpWire writes a sanitized JSON/hex observation when
// CURSOR_WIRE_DUMP_DIR is set. No-op otherwise. Does not change request
// semantics and never writes auth material.
func MaybeDumpWire(kind string, extra map[string]string, raw []byte, fields []WireField) {
	dir := strings.TrimSpace(os.Getenv("CURSOR_WIRE_DUMP_DIR"))
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	seq := wireDumpSeq.Add(1)
	ts := time.Now().UTC().Format("150405.000")
	safeKind := sanitizeDumpToken(kind)
	base := fmt.Sprintf("%s-%04d-%s", ts, seq, safeKind)
	hexPath := filepath.Join(dir, base+".hex")
	jsonPath := filepath.Join(dir, base+".json")
	_ = os.WriteFile(hexPath, []byte(hex.EncodeToString(raw)+"\n"), 0o644)
	payload := map[string]any{
		"event_type":   kind,
		"timestamp":    time.Now().UTC().Format(time.RFC3339Nano),
		"raw_hex_file": filepath.Base(hexPath),
		"raw_len":      len(raw),
		"raw_hex":      hex.EncodeToString(raw),
		"fields":       fields,
		"varints":      TurnEndedVarints(fields),
	}
	for k, v := range extra {
		if v == "" {
			continue
		}
		payload[k] = v
	}
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(jsonPath, append(b, '\n'), 0o644)
}

func sanitizeDumpToken(s string) string {
	if s == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "unknown"
	}
	if len(out) > 64 {
		return out[:64]
	}
	return out
}

// ConversationShort returns a short non-secret correlation token.
func ConversationShort(conversationID string) string {
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return ""
	}
	if len(conversationID) > 8 {
		return conversationID[:8]
	}
	return conversationID
}

// FormatIntMap is a stable debug rendering of field→value maps.
func FormatIntMap(m map[int]int64) string {
	if len(m) == 0 {
		return "{}"
	}
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, strconv.Itoa(k)+"="+strconv.FormatInt(m[k], 10))
	}
	return "{" + strings.Join(parts, " ") + "}"
}
