package claude

import "bytes"

func bytesSplit(raw []byte) [][]byte {
	var out [][]byte
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		out = append(out, line)
	}
	return out
}

func bytesContains(line, needle []byte) bool {
	return bytes.Contains(line, needle)
}
