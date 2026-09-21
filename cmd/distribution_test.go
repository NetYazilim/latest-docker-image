package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// fakeRegistryServer imitates OCI Distribution behaviour: the authentication
// challenge, the token endpoint, and a log of the requests.
type fakeRegistryServer struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []string // as "METHOD path"
	tokens   int      // how many times the token endpoint was called

	// handler serves the requests under /v2/, once the token has been checked.
	handler func(w http.ResponseWriter, r *http.Request)
}

func newFakeRegistry(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*distribution, *fakeRegistryServer) {
	t.Helper()

	f := &fakeRegistryServer{handler: handler}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		f.mu.Unlock()

		if r.URL.Path == "/token" {
			f.mu.Lock()
			f.tokens++
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"token":"T0K3N"}`)
			return
		}

		// Without a token, answer with a challenge: that is what real
		// registries do, and the realm must be read from this header rather
		// than hardcoded.
		if r.Header.Get("Authorization") != "Bearer T0K3N" {
			w.Header().Set("Www-Authenticate",
				fmt.Sprintf(`Bearer realm="%s/token",service="fake,service",scope="repository:x/y:pull"`, f.srv.URL))
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}`)
			return
		}

		f.handler(w, r)
	}))
	t.Cleanup(f.srv.Close)

	d := newDistribution("fake.registry")
	d.baseURL = f.srv.URL
	return d, f
}

func (f *fakeRegistryServer) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if strings.HasSuffix(r, path) {
			n++
		}
	}
	return n
}

func TestDistributionTokenAndPagination(t *testing.T) {
	d, f := newFakeRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("last") == "" {
			// A relative Link address: the driver must resolve it against the
			// current one.
			w.Header().Set("Link", `</v2/x/y/tags/list?n=100&last=2.0.0>; rel="next"`)
			fmt.Fprint(w, `{"name":"x/y","tags":["1.0.0","2.0.0"]}`)
			return
		}
		fmt.Fprint(w, `{"name":"x/y","tags":["3.0.0"]}`)
	})

	ctx := context.Background()
	pager := d.Tags("x/y")

	first, err := pager.Next(ctx)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if strings.Join(first, ",") != "1.0.0,2.0.0" {
		t.Errorf("first page = %v", first)
	}

	second, err := pager.Next(ctx)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if strings.Join(second, ",") != "3.0.0" {
		t.Errorf("second page = %v", second)
	}

	if done, err := pager.Next(ctx); err != nil || done != nil {
		t.Errorf("third call = (%v, %v), want (nil, nil)", done, err)
	}

	// The token must be taken once and cached, not fetched per request.
	if f.tokens != 1 {
		t.Errorf("token endpoint was called %d times, want 1", f.tokens)
	}
}

func TestDistributionInspectManifestIndex(t *testing.T) {
	d, _ := newFakeRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", mediaOCIIndex)
		w.Header().Set("Docker-Content-Digest", "sha256:deadbeef")
		fmt.Fprint(w, `{"mediaType":"`+mediaOCIIndex+`","manifests":[
			{"platform":{"architecture":"amd64","os":"linux"}},
			{"platform":{"architecture":"unknown","os":"unknown"}},
			{"platform":{"architecture":"arm64","os":"linux"}}]}`)
	})

	info, err := d.Inspect(context.Background(), "x/y", "1.0.0")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}

	if len(info.Platforms) != 2 {
		t.Fatalf("platform count = %d, want 2 (unknown should be dropped): %+v", len(info.Platforms), info.Platforms)
	}
	if !platformMatches(info, "amd64", "linux") || !platformMatches(info, "arm64", "linux") {
		t.Errorf("platforms = %+v", info.Platforms)
	}
	if platformMatches(info, "unknown", "unknown") {
		t.Error("an attestation record must not count as a platform")
	}
	if info.Digest != "sha256:deadbeef" {
		t.Errorf("Digest = %q", info.Digest)
	}
	// A multi-architecture index carries no date, which is why the output
	// falls back to the digest.
	if info.LastUpdated != "" {
		t.Errorf("LastUpdated = %q, want empty", info.LastUpdated)
	}
}

func TestDistributionInspectSingleManifest(t *testing.T) {
	d, f := newFakeRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/blobs/") {
			fmt.Fprint(w, `{"architecture":"amd64","os":"linux","created":"2024-05-06T07:08:09.12Z"}`)
			return
		}
		w.Header().Set("Docker-Content-Digest", "sha256:c0ffee")
		fmt.Fprint(w, `{"mediaType":"`+mediaDockerManifest+`","config":{"digest":"sha256:cfg"}}`)
	})

	info, err := d.Inspect(context.Background(), "x/y", "1.0.0")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}

	// On a single manifest the platform comes from the config blob: one extra
	// request.
	if f.count("/blobs/sha256:cfg") != 1 {
		t.Errorf("config blob requests = %d, want 1", f.count("/blobs/sha256:cfg"))
	}
	if !platformMatches(info, "amd64", "linux") {
		t.Errorf("platforms = %+v", info.Platforms)
	}
	if info.LastUpdated != "2024-05-06T07:08:09.12Z" {
		t.Errorf("LastUpdated = %q", info.LastUpdated)
	}
}

// TestDistributionResolveScansAllPages asserts that, because NewestFirst() is
// false, the pipeline scans every page, and that it fetches a manifest only for
// the candidate that passed the name filter.
func TestDistributionResolveScansAllPages(t *testing.T) {
	d, f := newFakeRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/manifests/") {
			w.Header().Set("Docker-Content-Digest", "sha256:abc")
			fmt.Fprint(w, `{"manifests":[{"platform":{"architecture":"amd64","os":"linux"}}]}`)
			return
		}
		if r.URL.Query().Get("last") == "" {
			w.Header().Set("Link", `</v2/x/y/tags/list?n=100&last=alpine>; rel="next"`)
			fmt.Fprint(w, `{"name":"x/y","tags":["1.0.0-beta","alpine"]}`)
			return
		}
		fmt.Fprint(w, `{"name":"x/y","tags":["9.1.0"]}`)
	})

	filter := regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	info, err := resolve(context.Background(), d, "x/y", filter, "amd64", "linux")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if info.Tag != "9.1.0" {
		t.Errorf("tag = %s, want 9.1.0 (on the second page)", info.Tag)
	}

	// Only the candidate may be fetched: 1.0.0-beta was excluded and alpine
	// did not pass the filter.
	if n := f.count("/manifests/9.1.0"); n != 1 {
		t.Errorf("9.1.0 manifest requests = %d, want 1", n)
	}
	for _, unwanted := range []string{"/manifests/1.0.0-beta", "/manifests/alpine"} {
		if n := f.count(unwanted); n != 0 {
			t.Errorf("%d requests were made for %s, expected none", n, unwanted)
		}
	}
}

func TestDistributionEmptyTagList(t *testing.T) {
	// An empty repository answers with "tags": null; since nil would mean "no
	// more pages", it has to become an empty page.
	d, _ := newFakeRegistry(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"x/y","tags":null}`)
	})

	page, err := d.Tags("x/y").Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if page == nil {
		t.Fatal("tags:null should yield an empty slice, not nil")
	}
	if len(page) != 0 {
		t.Errorf("page = %v", page)
	}
}

func TestDistributionStatusErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"404", http.StatusNotFound, `{"errors":[{"code":"NAME_UNKNOWN","message":"repository not found"}]}`, "repository not found"},
		{"403", http.StatusForbidden, `{"errors":[{"code":"DENIED","message":"subscription required"}]}`, "subscription required"},
		{"429", http.StatusTooManyRequests, `{}`, "rate limit"},
		{"500", http.StatusInternalServerError, ``, "HTTP 500"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := newFakeRegistry(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})

			_, err := d.Tags("x/y").Next(context.Background())
			if err == nil {
				t.Fatalf("expected an error for HTTP %d", tc.status)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, should contain %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "fake.registry") {
				t.Errorf("error = %q, should carry the registry name", err)
			}
		})
	}
}

func TestParseChallenge(t *testing.T) {
	scheme, params := parseChallenge(`Bearer realm="https://auth.example/token",service="reg,with,commas",scope="repository:a/b:pull"`)
	if !strings.EqualFold(scheme, "bearer") {
		t.Errorf("scheme = %q", scheme)
	}
	if params["realm"] != "https://auth.example/token" {
		t.Errorf("realm = %q", params["realm"])
	}
	// Commas inside quotes must not split a parameter.
	if params["service"] != "reg,with,commas" {
		t.Errorf("service = %q", params["service"])
	}
	if params["scope"] != "repository:a/b:pull" {
		t.Errorf("scope = %q", params["scope"])
	}

	if s, p := parseChallenge(""); s != "" || len(p) != 0 {
		t.Errorf("empty header = (%q, %v)", s, p)
	}
	if s, _ := parseChallenge("Basic"); s != "Basic" {
		t.Errorf("scheme without parameters = %q", s)
	}
}

func TestNextLink(t *testing.T) {
	base := "https://reg.example/v2/x/y/tags/list?n=100"

	tests := []struct {
		header string
		want   string
	}{
		{`</v2/x/y/tags/list?n=100&last=2.0.0>; rel="next"`, "https://reg.example/v2/x/y/tags/list?n=100&last=2.0.0"},
		{`<https://other.example/v2/x/y/tags/list?last=9>; rel="next"`, "https://other.example/v2/x/y/tags/list?last=9"},
		{`</a>; rel="prev", </b>; rel="next"`, "https://reg.example/b"},
		{`</a>; rel="prev"`, ""},
		{``, ""},
		{`malformed`, ""},
	}

	for _, tc := range tests {
		if got := nextLink(tc.header, base); got != tc.want {
			t.Errorf("nextLink(%q) = %q, want %q", tc.header, got, tc.want)
		}
	}
}
