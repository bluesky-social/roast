package record

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// ProviderResponsesFile is the append-only successful-response transcript.
// It records response JSON only, never request headers or credentials.
const ProviderResponsesFile = "provider-responses.jsonl"

// ResponseRecorder serializes model responses from concurrent verifier calls.
type ResponseRecorder struct {
	runID string
	mu    sync.Mutex
	seq   int
}

func NewResponseRecorder(runID string) *ResponseRecorder {
	return &ResponseRecorder{runID: runID}
}

// Append durably appends one valid provider JSON body.
func (r *ResponseRecorder) Append(role, protocol, model string, body json.RawMessage) error {
	if r == nil || r.runID == "" || filepath.Base(r.runID) != r.runID || r.runID == "." {
		return fmt.Errorf("record provider response: invalid run id")
	}
	if !json.Valid(body) {
		return fmt.Errorf("record provider response: body is not valid JSON")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	line, err := json.Marshal(struct {
		Sequence int             `json:"sequence"`
		Role     string          `json:"role"`
		Protocol string          `json:"protocol"`
		Model    string          `json:"model"`
		Body     json.RawMessage `json:"body"`
	}{r.seq, role, protocol, model, body})
	if err != nil {
		return fmt.Errorf("marshal provider response: %w", err)
	}
	dir := DefaultDir()
	if dir == "" {
		return fmt.Errorf("resolve record directory; set ROAST_RECORD_DIR to a writable directory")
	}
	runDir, err := makeRunDir(dir, r.runID)
	if err != nil {
		return err
	}
	path := filepath.Join(runDir, ProviderResponsesFile)
	f, err := openPrivateFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("append %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}
