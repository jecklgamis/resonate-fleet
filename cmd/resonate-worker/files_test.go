package main

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type upload struct{ name, content string }

func post(t *testing.T, srv *Server, scenario string, files ...upload) (*httptest.ResponseRecorder, http.Handler) {
	t.Helper()
	mux := http.NewServeMux()
	srv.Routes(mux)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	p, _ := mw.CreateFormFile("file", "scenario.yaml")
	p.Write([]byte(scenario))
	for _, f := range files {
		p, _ := mw.CreateFormFile("files", f.name)
		p.Write([]byte(f.content))
	}
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/task", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec, mux
}

const feederScenario = "protocol: http\nhttp:\n  feeder:\n    file: users.csv\n"

func TestDataFilesLandInTaskWorkspace(t *testing.T) {
	srv := newTestServer(t, 0)
	rec, mux := post(t, srv, feederScenario, upload{"users.csv", "id\n1\n2\n"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST /task = %d, body %s", rec.Code, rec.Body.String())
	}
	tasks := srv.Store.List()
	waitDone(t, mux, tasks[0].ID)
	got, err := os.ReadFile(filepath.Join(srv.Workspace, tasks[0].ID, "users.csv"))
	if err != nil || string(got) != "id\n1\n2\n" {
		t.Fatalf("users.csv = %q, %v", got, err)
	}
}

func TestDataFileRejections(t *testing.T) {
	tests := []struct {
		name     string
		scenario string
		files    []upload
		want     int
	}{
		{"reserved", feederScenario, []upload{{"scenario.yaml", "x"}}, 400},
		{"duplicate", feederScenario, []upload{{"users.csv", "x"}, {"users.csv", "y"}}, 400},
		{"too big", feederScenario, []upload{{"users.csv", strings.Repeat("x", maxDataFileBytes+1)}}, 413},
		{"absolute feeder", "http:\n  feeder:\n    file: /etc/passwd\n", nil, 400},
		{"dotdot body_file", "http:\n  targets:\n    - body_file: ../x\n", nil, 400},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t, 0)
			rec, _ := post(t, srv, tc.scenario, tc.files...)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d, body %s", rec.Code, tc.want, rec.Body.String())
			}
			if n := len(srv.Store.List()); n != 0 {
				t.Fatalf("%d tasks created for a rejected submission", n)
			}
			if es, _ := os.ReadDir(srv.Workspace); len(es) != 0 {
				t.Fatalf("workspace not cleaned up: %v", es)
			}
		})
	}
}

func TestTooManyDataFiles(t *testing.T) {
	var fs []upload
	for i := 0; i <= maxDataFiles; i++ {
		fs = append(fs, upload{string(rune('a'+i)) + ".csv", "x"})
	}
	rec, _ := post(t, newTestServer(t, 0), feederScenario, fs...)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestDataFileName(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "a/b.csv", `a\b.csv`, "scenario.yaml", "task.json", "log.txt"} {
		if _, err := dataFileName(bad); err == nil {
			t.Errorf("dataFileName(%q) accepted", bad)
		}
	}
	if _, err := dataFileName("users.csv"); err != nil {
		t.Errorf("dataFileName(users.csv) = %v", err)
	}
}

// Every scenario key that makes resonate read a local file must be refused when
// it points outside the task workspace, whichever protocol section it sits in.
func TestEveryFileKeyIsGuarded(t *testing.T) {
	bad := map[string]string{
		"message_file":  "ws:\n  messages:\n    - message_file: /etc/passwd\n",
		"ca_file":       "http:\n  tls:\n    ca_file: ../ca.pem\n",
		"cert_file":     "grpc:\n  tls:\n    cert_file: /etc/ssl/cert.pem\n",
		"key_file":      "http:\n  tls:\n    key_file: /root/.ssh/id_rsa\n",
		"proto_files":   "grpc:\n  proto_files:\n    - ok.proto\n    - /etc/shadow\n",
		"import_paths":  "grpc:\n  import_paths: [protos, ../..]\n",
		"body_file":     "http:\n  targets:\n    - body_file: /etc/hosts\n",
		"raw_body_file": "http:\n  targets:\n    - raw_body_file: ..\\x\n",
		"feeder.file":   "http:\n  feeder:\n    file: /etc/passwd\n",
	}
	for name, scenario := range bad {
		if err := checkScenarioPaths([]byte(scenario)); err == nil {
			t.Errorf("%s: an out-of-workspace path was accepted:\n%s", name, scenario)
		}
	}
	good := "grpc:\n  proto_files: [svc.proto]\n  import_paths: [protos]\n  tls:\n    ca_file: ca.pem\n    cert_file: c.pem\n    key_file: k.pem\nws:\n  messages:\n    - message_file: msg.json\nhttp:\n  feeder:\n    file: users.csv\n    rows: [{a: b}]\n"
	if err := checkScenarioPaths([]byte(good)); err != nil {
		t.Errorf("plain workspace-relative names were refused: %v", err)
	}
}
