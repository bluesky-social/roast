package record

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestResponseRecorderSerializesConcurrentResponses(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ROAST_RECORD_DIR", dir)
	recorder := NewResponseRecorder("20260920T120000Z-1234abcd")
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := recorder.Append("verifier", "responses", "model", json.RawMessage(`{"id":"response"}`)); err != nil {
				t.Errorf("Append: %v", err)
			}
		}()
	}
	wg.Wait()
	data, err := os.ReadFile(filepath.Join(dir, "runs", recorder.runID, ProviderResponsesFile))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 8 {
		t.Fatalf("response lines = %d, want 8", len(lines))
	}
	for i, line := range lines {
		var entry struct {
			Sequence int             `json:"sequence"`
			Body     json.RawMessage `json:"body"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		if entry.Sequence != i+1 || string(entry.Body) != `{"id":"response"}` {
			t.Fatalf("entry %d = %#v", i, entry)
		}
	}
}
