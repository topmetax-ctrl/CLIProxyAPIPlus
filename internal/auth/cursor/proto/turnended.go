package proto

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// TurnEndedUpdate nested field numbers from Cursor.app generated
// agent.v1.TurnEndedUpdate (protobuf-es makeMessageType). The embedded
// alma-plugins descriptor in this repo lists the message as empty.
const (
	TE_InputTokens      = 1 // input_tokens, scalar int64
	TE_OutputTokens     = 2 // output_tokens, scalar int64
	TE_CacheReadTokens  = 3 // cache_read_tokens, scalar int64
	TE_CacheWriteTokens = 4 // cache_write_tokens, scalar int64
	TE_ReasoningTokens  = 5 // reasoning_tokens, scalar int64
)

// TurnEndedUsage is the canonical Cursor terminal usage decoded from
// InteractionUpdate field 14. Only fields present on the wire are marked
// present. Absent optional varints stay unset; they are not synthesized as 0.
type TurnEndedUsage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64

	HasInput      bool
	HasOutput     bool
	HasCacheRead  bool
	HasCacheWrite bool
	HasReasoning  bool

	Unknown map[int]int64
	Raw     []byte
}

// HasAny reports whether at least one known or unknown field was present.
func (u TurnEndedUsage) HasAny() bool {
	return u.HasInput || u.HasOutput || u.HasCacheRead || u.HasCacheWrite || u.HasReasoning || len(u.Unknown) > 0
}

// DecodeTurnEndedUsage maps inspected TurnEndedUpdate fields onto the
// Cursor.app schema. Unknown field numbers are preserved, not renamed.
func DecodeTurnEndedUsage(raw []byte, fields []WireField) TurnEndedUsage {
	out := TurnEndedUsage{
		Raw:     append([]byte(nil), raw...),
		Unknown: map[int]int64{},
	}
	if len(fields) == 0 && len(raw) > 0 {
		fields = InspectProtoFields(raw)
	}
	for _, field := range fields {
		if field.Varint == nil || field.WireType != "varint" {
			if field.Number > 0 {
				out.Unknown[field.Number] = 0
			}
			continue
		}
		switch field.Number {
		case TE_InputTokens:
			out.InputTokens = *field.Varint
			out.HasInput = true
		case TE_OutputTokens:
			out.OutputTokens = *field.Varint
			out.HasOutput = true
		case TE_CacheReadTokens:
			out.CacheReadTokens = *field.Varint
			out.HasCacheRead = true
		case TE_CacheWriteTokens:
			out.CacheWriteTokens = *field.Varint
			out.HasCacheWrite = true
		case TE_ReasoningTokens:
			out.ReasoningTokens = *field.Varint
			out.HasReasoning = true
		default:
			out.Unknown[field.Number] = *field.Varint
		}
	}
	return out
}

// FormatUnknownFields is a stable debug rendering of leftover field numbers.
func (u TurnEndedUsage) FormatUnknownFields() string {
	if len(u.Unknown) == 0 {
		return "{}"
	}
	keys := make([]int, 0, len(u.Unknown))
	for k := range u.Unknown {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, strconv.Itoa(k)+"="+strconv.FormatInt(u.Unknown[k], 10))
	}
	return "{" + strings.Join(parts, " ") + "}"
}

// DebugSummary is a secret-free log line for terminal usage.
func (u TurnEndedUsage) DebugSummary() string {
	return fmt.Sprintf("input=%s output=%s cache_read=%s cache_write=%s reasoning=%s unknown=%s",
		optionalInt(u.HasInput, u.InputTokens),
		optionalInt(u.HasOutput, u.OutputTokens),
		optionalInt(u.HasCacheRead, u.CacheReadTokens),
		optionalInt(u.HasCacheWrite, u.CacheWriteTokens),
		optionalInt(u.HasReasoning, u.ReasoningTokens),
		u.FormatUnknownFields(),
	)
}

func optionalInt(present bool, value int64) string {
	if !present {
		return "absent"
	}
	return strconv.FormatInt(value, 10)
}
