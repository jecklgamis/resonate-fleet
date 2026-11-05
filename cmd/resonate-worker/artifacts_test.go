package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type fakePutter struct {
	keys map[string]string
	fail string
}

func (f *fakePutter) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	if f.fail != "" && *in.Key == f.fail {
		return nil, errors.New("boom")
	}
	b, _ := io.ReadAll(in.Body)
	f.keys[*in.Key] = string(b)
	return &s3.PutObjectOutput{}, nil
}

func TestS3StoreUploadsArtifactsButNotTaskState(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"scenario.yaml": "s", "report.json": "r", "users.csv": "u", taskFile: "state"} {
		os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
	}
	os.Mkdir(filepath.Join(dir, "sub"), 0o755)

	fp := &fakePutter{keys: map[string]string{}}
	st := &S3Store{Bucket: "b", Prefix: "runs", client: fp}
	loc, err := st.Upload(context.Background(), "abc", dir)
	if err != nil {
		t.Fatal(err)
	}
	if loc != "s3://b/runs/abc/" {
		t.Fatalf("location = %q", loc)
	}
	var got []string
	for k := range fp.keys {
		got = append(got, k)
	}
	sort.Strings(got)
	want := []string{"runs/abc/report.json", "runs/abc/scenario.yaml", "runs/abc/users.csv"}
	if len(got) != len(want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("keys = %v, want %v", got, want)
		}
	}
}

func TestS3StoreNoPrefixAndError(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "log.txt"), []byte("l"), 0o644)
	st := &S3Store{Bucket: "b", client: &fakePutter{keys: map[string]string{}, fail: "abc/log.txt"}}
	if _, err := st.Upload(context.Background(), "abc", dir); err == nil {
		t.Fatal("want error")
	}
}

type fakeStore struct{ err error }

func (f fakeStore) Upload(_ context.Context, id, _ string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return "s3://b/" + id + "/", nil
}

func TestRunnerRecordsUploadOutcome(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		wantURL bool
	}{{"ok", nil, true}, {"upload fails", errors.New("denied"), false}} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t, 0)
			srv.Runner.Artifacts = fakeStore{err: tc.err}
			rec, mux := post(t, srv, "protocol: http\n")
			if rec.Code != 202 {
				t.Fatalf("POST = %d", rec.Code)
			}
			s := waitDone(t, mux, srv.Store.List()[0].ID)
			if s.Status != StatusDone {
				t.Fatalf("status = %s, want done even if upload fails", s.Status)
			}
			if (s.ArtifactsURL != "") != tc.wantURL || (s.UploadError != "") == tc.wantURL {
				t.Fatalf("snapshot = %+v", s)
			}
		})
	}
}
