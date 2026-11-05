package main

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"sync"
	"time"
)

// Entry is one registered worker. Workers re-register periodically as a
// heartbeat; one that stops is dropped after the registry's TTL.
type Entry struct {
	Name     string            `json:"name"`
	URL      string            `json:"url"`
	Labels   map[string]string `json:"labels,omitempty"`
	LastSeen time.Time         `json:"last_seen"`

	// Token is the worker's own API token, sent by the controller on every call
	// to it. Never serialized into listings.
	Token string `json:"-"`
}

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// Registry is the set of live workers, in memory only: after a controller
// restart it refills within one heartbeat interval.
type Registry struct {
	TTL time.Duration

	mu  sync.Mutex
	now func() time.Time
	m   map[string]Entry
}

func NewRegistry(ttl time.Duration) *Registry {
	return &Registry{TTL: ttl, now: time.Now, m: map[string]Entry{}}
}

// Register adds or refreshes a worker. Name is used in URLs and as an output
// directory, so it is restricted to a plain token.
func (r *Registry) Register(e Entry) error {
	if !validName.MatchString(e.Name) {
		return fmt.Errorf("invalid worker name %q: want letters, digits, '.', '_' or '-'", e.Name)
	}
	u, err := url.Parse(e.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return fmt.Errorf("invalid worker url %q: want http(s)://host[:port]", e.URL)
	}
	e.LastSeen = r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[e.Name] = e
	return nil
}

func (r *Registry) Remove(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, name)
}

// Live returns the workers seen within the TTL whose labels include every
// key=value in selector, sorted by name. Expired entries are dropped.
func (r *Registry) Live(selector map[string]string) []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := r.now().Add(-r.TTL)
	var out []Entry
	for name, e := range r.m {
		if e.LastSeen.Before(cutoff) {
			delete(r.m, name)
			continue
		}
		if matches(e.Labels, selector) {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func matches(labels, selector map[string]string) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}
