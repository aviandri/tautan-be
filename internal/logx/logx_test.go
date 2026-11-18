package logx

import (
	"bytes"
	"encoding/json"
	"log"
	"testing"
)

func TestJSONRoutesStdLog(t *testing.T) {
	var buf bytes.Buffer
	setup(&buf, "json", "info")
	log.Printf("hello %d", 42)
	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("not JSON: %q (%v)", buf.String(), err)
	}
	if rec["msg"] != "hello 42" || rec["level"] != "INFO" {
		t.Fatalf("unexpected record %v", rec)
	}
}

func TestLevelFilters(t *testing.T) {
	var buf bytes.Buffer
	l := setup(&buf, "json", "warn")
	l.Info("dropped")
	if buf.Len() != 0 {
		t.Fatalf("info logged at warn level: %q", buf.String())
	}
}
