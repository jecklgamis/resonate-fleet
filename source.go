package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// Errors a SourceFetcher returns, so the HTTP layer can map each to a status.
var (
	errSourceDisabled   = errors.New("scenario URLs are disabled: set SCENARIO_SOURCES to the URL prefixes this worker may fetch from")
	errSourceBadURL     = errors.New("invalid scenario URL")
	errSourceNotAllowed = errors.New("scenario URL is not under an allowed source (SCENARIO_SOURCES)")
	errSourceTooLarge   = errors.New("scenario is larger than the size limit")
)

// SourceFetcher downloads a scenario file by URL, for submissions that say where
// the scenario lives instead of uploading it.
//
// A worker that fetches whatever URL it is handed can be pointed at internal
// services, so fetching is off unless Allow is non-empty, and every URL —
// including each redirect hop — must match one of its entries.
type SourceFetcher struct {
	Allow    []string // allowed prefixes: scheme://host[:port][/path-prefix]
	Token    string   // optional bearer token sent with every fetch
	MaxBytes int64    // size cap; 0 means defaultMaxScenarioBytes
	Timeout  time.Duration

	client *http.Client
}

const (
	defaultMaxScenarioBytes = 1 << 20 // 1 MiB: a scenario is a small YAML file
	defaultFetchTimeout     = 10 * time.Second
	maxRedirects            = 3
)

// NewSourceFetcher builds a fetcher from the allowed prefixes. Entries that
// aren't http(s) URLs are rejected here, at startup, rather than silently never
// matching.
func NewSourceFetcher(allow []string, token string) (*SourceFetcher, error) {
	var clean []string
	for _, a := range allow {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		u, err := url.Parse(a)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			return nil, fmt.Errorf("SCENARIO_SOURCES entry %q: want http(s)://host[:port][/path-prefix]", a)
		}
		clean = append(clean, a)
	}
	f := &SourceFetcher{Allow: clean, Token: token}
	f.client = &http.Client{
		Timeout: f.timeout(),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				return errors.New("too many redirects")
			}
			if !f.allowed(req.URL) {
				return fmt.Errorf("redirect to %s: %w", req.URL.Redacted(), errSourceNotAllowed)
			}
			return nil
		},
	}
	return f, nil
}

func (f *SourceFetcher) timeout() time.Duration {
	if f.Timeout > 0 {
		return f.Timeout
	}
	return defaultFetchTimeout
}

func (f *SourceFetcher) maxBytes() int64 {
	if f.MaxBytes > 0 {
		return f.MaxBytes
	}
	return defaultMaxScenarioBytes
}

// allowed reports whether u falls under one of the allowed prefixes: same scheme
// and host (so "config.example.com.evil.com" never matches "config.example.com"),
// and a path that is the prefix itself or lies beneath it on a segment boundary
// (so "/scenarios-evil" doesn't match "/scenarios"). The path is cleaned first,
// so "/scenarios/../secret" can't climb out of the prefix.
func (f *SourceFetcher) allowed(u *url.URL) bool {
	if u.User != nil {
		return false
	}
	p := path.Clean("/" + u.Path)
	for _, a := range f.Allow {
		au, err := url.Parse(a)
		if err != nil {
			continue
		}
		if !strings.EqualFold(au.Scheme, u.Scheme) || !strings.EqualFold(au.Host, u.Host) {
			continue
		}
		prefix := strings.TrimRight(path.Clean("/"+au.Path), "/")
		if prefix == "" || p == prefix || strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return false
}

// Fetch downloads raw and returns the body. The body is returned as a reader
// capped at the size limit; a body over the limit makes Read fail with
// errSourceTooLarge. The caller closes it.
func (f *SourceFetcher) Fetch(raw string) (io.ReadCloser, error) {
	if f == nil || len(f.Allow) == 0 {
		return nil, errSourceDisabled
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("%w: %q", errSourceBadURL, raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%w: unsupported scheme %q (only http and https)", errSourceBadURL, u.Scheme)
	}
	if !f.allowed(u) {
		return nil, errSourceNotAllowed
	}

	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errSourceBadURL, err)
	}
	if f.Token != "" {
		req.Header.Set("Authorization", "Bearer "+f.Token)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching scenario: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		resp.Body.Close()
		return nil, fmt.Errorf("fetching scenario: %s returned %s", u.Redacted(), resp.Status)
	}
	return &limitedBody{rc: resp.Body, left: f.maxBytes()}, nil
}

// limitedBody reads at most left bytes and fails, rather than silently
// truncating, if the source has more: a cut-off YAML file would run as a
// different scenario than the one that was published.
type limitedBody struct {
	rc   io.ReadCloser
	left int64
}

func (l *limitedBody) Read(p []byte) (int, error) {
	if l.left <= 0 {
		var one [1]byte
		if n, _ := l.rc.Read(one[:]); n > 0 {
			return 0, errSourceTooLarge
		}
		return 0, io.EOF
	}
	if int64(len(p)) > l.left {
		p = p[:l.left]
	}
	n, err := l.rc.Read(p)
	l.left -= int64(n)
	return n, err
}

func (l *limitedBody) Close() error { return l.rc.Close() }

// scenarioName is the display name for a fetched scenario: the last path segment
// of its URL, or "scenario.yaml" if there isn't one.
func scenarioName(raw string) string {
	u, err := url.Parse(raw)
	if err == nil {
		if base := path.Base(path.Clean("/" + u.Path)); base != "/" && base != "." {
			return base
		}
	}
	return "scenario.yaml"
}
