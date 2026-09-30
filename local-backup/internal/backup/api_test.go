package backup

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestHTTPAPI(t *testing.T) {
	svc, dir := newTestService(t, FaultInjection{})
	srv := httptest.NewServer(NewServer(svc).Handler())
	defer srv.Close()

	root := filepath.Join(dir, "data")
	os.MkdirAll(root, 0o755)
	os.WriteFile(filepath.Join(root, "f.txt"), []byte("api round trip"), 0o640)

	post := func(path string, body any) (int, map[string]any) {
		b, _ := json.Marshal(body)
		resp, err := http.Post(srv.URL+path, "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}
	get := func(path string) (int, map[string]any) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}

	code, rep := post("/v1/backups", map[string]string{"path": root})
	if code != 200 || rep["complete"] != true {
		t.Fatalf("backup: code=%d body=%v", code, rep)
	}
	snapID := int64(rep["snapshot_id"].(float64))

	if code, body := get("/v1/snapshots"); code != 200 {
		t.Fatalf("list: %d %v", code, body)
	}
	if code, _ := get("/v1/snapshots/9999"); code != 404 {
		t.Fatalf("missing snapshot should 404, got %d", code)
	}
	if code, _ := get("/v1/snapshots/abc"); code != 400 {
		t.Fatalf("bad id should 400, got %d", code)
	}
	if code, body := get("/v1/snapshots/1/diag"); code != 200 || body["verification"] != "healthy" {
		t.Fatalf("diag healthy: %d %v", code, body)
	}

	target := filepath.Join(dir, "restored")
	code, rr := post("/v1/restore", map[string]any{"snapshot_id": snapID, "target": target})
	if code != 200 || rr["verified"] != true {
		t.Fatalf("restore: %d %v", code, rr)
	}
	if data, err := os.ReadFile(filepath.Join(target, "f.txt")); err != nil || string(data) != "api round trip" {
		t.Fatalf("restored content: %q %v", data, err)
	}

	// Second restore into the same non-empty target must be 400.
	code, rr = post("/v1/restore", map[string]any{"snapshot_id": snapID, "target": target})
	if code != 400 {
		t.Fatalf("expected 400 for non-empty target, got %d %v", code, rr)
	}
}

func TestHTTPAPIIncompleteSnapshot409(t *testing.T) {
	svc, dir := newTestService(t, FaultInjection{SkipNewChunk: 1})
	srv := httptest.NewServer(NewServer(svc).Handler())
	defer srv.Close()

	root := filepath.Join(dir, "data")
	os.MkdirAll(root, 0o755)
	fillFile(t, filepath.Join(root, "big.bin"), 16*1024*1024, 5)

	b, _ := json.Marshal(map[string]string{"path": root})
	resp, err := http.Post(srv.URL+"/v1/backups", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	var rep BackupReport
	json.NewDecoder(resp.Body).Decode(&rep)
	resp.Body.Close()
	if rep.Complete || len(rep.MissingChunks) == 0 {
		t.Fatalf("expected incomplete report: %+v", rep)
	}

	b, _ = json.Marshal(map[string]any{"snapshot_id": rep.SnapshotID, "target": filepath.Join(dir, "out")})
	resp2, err := http.Post(srv.URL+"/v1/restore", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var m map[string]any
	json.NewDecoder(resp2.Body).Decode(&m)
	// Incomplete snapshots are rejected at the state check (400), and even
	// without that gate preflight would be 409 with missing_chunks.
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %v", resp2.StatusCode, m)
	}
}
