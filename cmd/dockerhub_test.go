package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp/syntax"
	"strings"
	"testing"
)

func TestHubPagerFollowsNext(t *testing.T) {
	var srv *httptest.Server
	var paths []string
	page := 0

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		page++
		w.Header().Set("Content-Type", "application/json")
		if page == 1 {
			fmt.Fprintf(w, `{"next":%q,"results":[
				{"name":"1.0.0","content_type":"image","last_updated":"2024-01-02T03:04:05.678Z",
				 "images":[{"architecture":"amd64","os":"linux","status":"active"}]}]}`,
				srv.URL+"/sayfa2")
			return
		}
		fmt.Fprint(w, `{"next":"","results":[
			{"name":"2.0.0","content_type":"image",
			 "images":[{"architecture":"arm64","os":"linux","status":"active"},
			           {"architecture":"amd64","os":"linux","status":"inactive"}]}]}`)
	}))
	defer srv.Close()

	ctx := context.Background()
	hub := newDockerHub("")
	hub.baseURL = srv.URL

	pager := hub.Tags("traefik")

	first, err := pager.Next(ctx)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(first) != 1 || first[0] != "1.0.0" {
		t.Fatalf("first page = %v, want [1.0.0]", first)
	}

	second, err := pager.Next(ctx)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(second) != 1 || second[0] != "2.0.0" {
		t.Fatalf("second page = %v, want [2.0.0]", second)
	}

	if done, err := pager.Next(ctx); err != nil || done != nil {
		t.Fatalf("third call = (%v, %v), want (nil, nil)", done, err)
	}

	// A single-component name must be moved into the "library" namespace.
	if !strings.Contains(paths[0], "/v2/repositories/library/traefik/tags") {
		t.Errorf("request path = %q, expected the library/ prefix", paths[0])
	}

	// Records seen while paging must be kept for Inspect: on this driver
	// Inspect never hits the network.
	info, err := hub.Inspect(ctx, "traefik", "1.0.0")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if info.LastUpdated != "2024-01-02T03:04:05.678Z" {
		t.Errorf("LastUpdated = %q", info.LastUpdated)
	}
	if !platformMatches(info, "amd64", "linux") {
		t.Errorf("amd64/linux should have matched: %+v", info.Platforms)
	}

	// An image with status != active must not enter the platform list.
	info, err = hub.Inspect(ctx, "traefik", "2.0.0")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if platformMatches(info, "amd64", "linux") {
		t.Errorf("an inactive image should not have matched: %+v", info.Platforms)
	}
	if !platformMatches(info, "arm64", "linux") {
		t.Errorf("arm64/linux should have matched: %+v", info.Platforms)
	}
}

func TestHubInspectPluginIgnoresPlatform(t *testing.T) {
	hub := newDockerHub("")
	hub.seen["1.0.0"] = hubTag{Name: "1.0.0", ContentType: "plugin"}

	info, err := hub.Inspect(context.Background(), "x/y", "1.0.0")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !info.AnyPlatform {
		t.Error("a plugin record should have been marked AnyPlatform")
	}
	if !platformMatches(info, "s390x", "linux") {
		t.Error("a plugin should pass every platform")
	}
}

func TestHubFetchSurfacesAPIMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"object not found"}`)
	}))
	defer srv.Close()

	hub := newDockerHub("")
	hub.baseURL = srv.URL

	_, err := hub.Tags("x/y").Next(context.Background())
	if err == nil {
		t.Fatal("expected an error for 404")
	}
	if !strings.Contains(err.Error(), "object not found") {
		t.Errorf("error = %q, should carry the API message", err)
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error = %q, should carry the status code", err)
	}
}

func TestApiMessage(t *testing.T) {
	tests := []struct {
		body string
		want string
	}{
		{`{"message":"rate limit"}`, "rate limit"},
		{`{"detail":"not found"}`, "not found"},
		{`{"message":"m","detail":"d"}`, "m"},
		{`{}`, ""},
		{`malformed json`, ""},
	}

	for _, tc := range tests {
		if got := apiMessage([]byte(tc.body)); got != tc.want {
			t.Errorf("apiMessage(%s) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

func TestRequiredLiteral(t *testing.T) {
	tests := []struct {
		re   string
		want string
	}{
		{`(\d+)\.(\d+)\.(\d+)$`, ""},
		{`(\d+)\.(\d+)\.(\d+)-alpine$`, "-alpine"},
		{`-alpine$`, "-alpine"},
		{`v(\d+)-jammy`, "-jammy"},
		{`^(\d+)\.(\d+)\.(\d+)-alpine$`, "-alpine"},
		// Alternation: no branch is required, so no literal may be used.
		{`alpine|bookworm`, ""},
		{`(alpine|bookworm)$`, ""},
		// An optional group is not required.
		{`(-alpine)?$`, ""},
		// A case-folded literal may not agree with the server-side filter.
		{`(?i)-ALPINE$`, ""},
		{``, ""},
		{`.*`, ""},
	}

	for _, tc := range tests {
		rep, err := syntax.Parse(tc.re, syntax.Perl)
		if err != nil {
			t.Fatalf("%q could not be parsed: %v", tc.re, err)
		}
		if got := requiredLiteral(rep.Simplify()); got != tc.want {
			t.Errorf("requiredLiteral(%q) = %q, want %q", tc.re, got, tc.want)
		}
	}
}
