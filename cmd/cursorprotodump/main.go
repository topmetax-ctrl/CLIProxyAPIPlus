// Command cursorprotodump prints the Cursor agent.proto descriptor as text so
// audits can diff the wire contract against what the encoder actually sets.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"

	cursorproto "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor/proto"
)

func main() {
	only := flag.String("msg", "", "comma-separated message names to dump (default: all)")
	grep := flag.String("grep", "", "case-insensitive substring filter on message name")
	flag.Parse()

	fd := cursorproto.AgentFileDescriptor()
	wanted := map[string]bool{}
	if strings.TrimSpace(*only) != "" {
		for _, n := range strings.Split(*only, ",") {
			wanted[strings.TrimSpace(n)] = true
		}
	}

	msgs := fd.Messages()
	names := make([]string, 0, msgs.Len())
	byName := map[string]protoreflect.MessageDescriptor{}
	for i := 0; i < msgs.Len(); i++ {
		md := msgs.Get(i)
		n := string(md.Name())
		names = append(names, n)
		byName[n] = md
	}
	sort.Strings(names)

	out := os.Stdout
	fmt.Fprintf(out, "# agent.proto descriptor dump\n")
	fmt.Fprintf(out, "# package=%s total_top_level_messages=%d total_enums=%d\n\n", fd.Package(), msgs.Len(), fd.Enums().Len())

	for _, n := range names {
		if len(wanted) > 0 && !wanted[n] {
			continue
		}
		if *grep != "" && !strings.Contains(strings.ToLower(n), strings.ToLower(*grep)) {
			continue
		}
		dumpMessage(out, byName[n], 0)
	}
}

func dumpMessage(out *os.File, md protoreflect.MessageDescriptor, depth int) {
	indent := strings.Repeat("  ", depth)
	fmt.Fprintf(out, "%smessage %s {\n", indent, md.Name())
	fields := md.Fields()
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		label := ""
		switch {
		case f.IsList():
			label = "repeated "
		case f.IsMap():
			label = "map "
		case f.HasOptionalKeyword():
			label = "optional "
		}
		typeName := f.Kind().String()
		switch f.Kind() {
		case protoreflect.MessageKind, protoreflect.GroupKind:
			typeName = string(f.Message().FullName())
		case protoreflect.EnumKind:
			typeName = string(f.Enum().FullName())
		}
		oneof := ""
		if od := f.ContainingOneof(); od != nil && !od.IsSynthetic() {
			oneof = fmt.Sprintf("  // oneof %s", od.Name())
		}
		fmt.Fprintf(out, "%s  %s%s %s = %d;%s\n", indent, label, typeName, f.Name(), f.Number(), oneof)
	}
	nested := md.Messages()
	for i := 0; i < nested.Len(); i++ {
		if nested.Get(i).IsMapEntry() {
			continue
		}
		dumpMessage(out, nested.Get(i), depth+1)
	}
	fmt.Fprintf(out, "%s}\n\n", indent)
}
