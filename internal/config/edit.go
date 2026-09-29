package config

import (
	"bytes"
	"strconv"
	"strings"
)

// SetString returns data with section.key set to the TOML string value,
// editing the text in place so comments and the rest of the file survive
// (a decode/encode round trip would drop them):
//
//   - an existing `key = ...` line inside [section] is replaced;
//   - otherwise the line is added right after the [section] header;
//   - otherwise a new [section] with the key is appended.
//
// It handles the files backpack writes (`config init`, first-run setup): one table per section, no dotted keys or inline tables. The
// caller should validate the result with Parse before writing it.
func SetString(data []byte, section, key, value string) []byte {
	line := key + " = " + strconv.Quote(value)
	lines := strings.SplitAfter(string(data), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	header := -1
	in := false
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") {
			in = t == "["+section+"]"
			if in {
				header = i
			}
			continue
		}
		if !in {
			continue
		}
		k, _, ok := strings.Cut(t, "=")
		if ok && strings.TrimSpace(k) == key {
			lines[i] = line + "\n"
			return []byte(strings.Join(lines, ""))
		}
	}
	var b bytes.Buffer
	if header >= 0 {
		for i, l := range lines {
			b.WriteString(l)
			if i == header {
				if !strings.HasSuffix(l, "\n") {
					b.WriteByte('\n')
				}
				b.WriteString(line + "\n")
			}
		}
		return b.Bytes()
	}
	b.WriteString(strings.Join(lines, ""))
	if b.Len() > 0 && !bytes.HasSuffix(b.Bytes(), []byte("\n")) {
		b.WriteByte('\n')
	}
	if b.Len() > 0 {
		b.WriteByte('\n')
	}
	b.WriteString("[" + section + "]\n" + line + "\n")
	return b.Bytes()
}
