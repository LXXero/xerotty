package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestTabIDParamDecode pins the lenient tab_id forms and — most
// importantly — the TEACHING rejection of aggregator-namespaced ids,
// which agents end up holding when they confuse the two MCP sockets'
// schemas.
func TestTabIDParamDecode(t *testing.T) {
	var id tabIDParam
	if err := json.Unmarshal([]byte(`3`), &id); err != nil || id != 3 {
		t.Fatalf("number: %v %d", err, id)
	}
	if err := json.Unmarshal([]byte(`"7"`), &id); err != nil || id != 7 {
		t.Fatalf("numeric string: %v %d", err, id)
	}
	err := json.Unmarshal([]byte(`"local:3"`), &id)
	if err == nil || !strings.Contains(err.Error(), "aggregator") {
		t.Fatalf("namespaced id must teach the schema split, got %v", err)
	}
	if err := json.Unmarshal([]byte(`"abc"`), &id); err == nil {
		t.Fatal("garbage string must error")
	}
}
