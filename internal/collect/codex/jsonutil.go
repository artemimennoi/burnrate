package codex

import "encoding/json"

func validJSON(line []byte) bool {
	var v any
	return json.Unmarshal(line, &v) == nil
}
