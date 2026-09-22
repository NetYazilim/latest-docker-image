package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// rhServer stands in for the catalogue. It answers every request with the body
// for the page asked for, and records the query it was given.
func rhServer(t *testing.T, pages ...string) (*redHat, *[]url.Values) {
	t.Helper()
	var queries []url.Values

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query())
		n := 0
		fmt.Sscanf(r.URL.Query().Get("page"), "%d", &n)
		w.Header().Set("Content-Type", "application/json")
		if n >= len(pages) {
			fmt.Fprint(w, `{"data":[],"total":0}`)
			return
		}
		fmt.Fprint(w, pages[n])
	}))
	t.Cleanup(srv.Close)

	rh := newRedHat("amd64")
	rh.baseURL = srv.URL
	return rh, &queries
}

// build renders one image record.
func build(arch, created, repo, digest string, tags ...string) string {
	var ts []string
	for _, tag := range tags {
		ts = append(ts, fmt.Sprintf(`{"name":%q,"added_date":%q}`, tag, created))
	}
	return fmt.Sprintf(`{"architecture":%q,"creation_date":%q,"repositories":[
		{"registry":"registry.access.redhat.com","repository":%q,"published":true,
		 "manifest_list_digest":%q,"tags":[%s]}]}`,
		arch, created, repo, digest, strings.Join(ts, ","))
}

func page(total int, records ...string) string {
	return fmt.Sprintf(`{"data":[%s],"total":%d}`, strings.Join(records, ","), total)
}

func TestRedHatQueryShape(t *testing.T) {
	rh, queries := rhServer(t, page(1, build("amd64", "2026-09-22T09:45:00Z", "ubi9/ubi", "sha256:aa", "9.8")))

	if _, err := rh.Tags("ubi9/ubi").Next(context.Background()); err != nil {
		t.Fatalf("Next: %v", err)
	}

	q := (*queries)[0]
	for field, want := range map[string]string{
		"page":      "0",
		"page_size": "100",
		"sort_by":   "creation_date[desc]",
		"filter":    "architecture==amd64",
		"include":   "data.architecture,data.creation_date,data.repositories",
	} {
		if got := q.Get(field); got != want {
			t.Errorf("%s = %q, want %q", field, got, want)
		}
	}
}

// A tag published for several architectures arrives as one record per
// architecture, and the platform list has to end up carrying all of them.
func TestRedHatMergesArchitectures(t *testing.T) {
	rh, _ := rhServer(t, page(2,
		build("amd64", "2026-09-22T09:45:00Z", "ubi9/ubi", "sha256:aa", "9.8"),
		build("arm64", "2026-09-22T09:45:00Z", "ubi9/ubi", "sha256:aa", "9.8"),
	))

	ctx := context.Background()
	names, err := rh.Tags("ubi9/ubi").Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if len(names) != 1 || names[0] != "9.8" {
		t.Fatalf("names = %v, want [9.8] once", names)
	}

	info, err := rh.Inspect(ctx, "ubi9/ubi", "9.8")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if len(info.Platforms) != 2 {
		t.Fatalf("platforms = %v, want amd64 and arm64", info.Platforms)
	}
	if !platformMatches(info, "amd64", "linux") || !platformMatches(info, "arm64", "linux") {
		t.Errorf("platforms = %v, want both to match linux", info.Platforms)
	}
	if info.Digest != "sha256:aa" {
		t.Errorf("digest = %q, want sha256:aa", info.Digest)
	}
	if info.LastUpdated != "2026-09-22T09:45:00Z" {
		t.Errorf("last updated = %q", info.LastUpdated)
	}
}

// One build is published to several repositories: ubi9/ubi and ubi9 both carry
// the same image. Only the repository that was asked for may contribute tags.
func TestRedHatIgnoresOtherRepositories(t *testing.T) {
	body := `{"data":[{"architecture":"amd64","creation_date":"2026-09-22T09:45:00Z","repositories":[
		{"registry":"registry.access.redhat.com","repository":"ubi9","published":true,
		 "manifest_list_digest":"sha256:bb","tags":[{"name":"yanlis","added_date":"2026-09-22T09:45:00Z"}]},
		{"registry":"quay.io","repository":"ubi9/ubi","published":true,
		 "manifest_list_digest":"sha256:cc","tags":[{"name":"yabanci","added_date":"2026-09-22T09:45:00Z"}]},
		{"registry":"registry.access.redhat.com","repository":"ubi9/ubi","published":true,
		 "manifest_list_digest":"sha256:aa","tags":[{"name":"9.8","added_date":"2026-09-22T09:45:00Z"}]}]}],
		"total":1}`

	rh, _ := rhServer(t, body)
	names, err := rh.Tags("ubi9/ubi").Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if len(names) != 1 || names[0] != "9.8" {
		t.Fatalf("names = %v, want only [9.8]", names)
	}
}

// manifest_list_digest is the multi-architecture digest and is preferred; a
// single-architecture build only has manifest_schema2_digest.
func TestRedHatDigestFallsBackToSchema2(t *testing.T) {
	body := `{"data":[{"architecture":"amd64","creation_date":"2026-01-01T00:00:00Z","repositories":[
		{"registry":"registry.access.redhat.com","repository":"ubi9/ubi","published":true,
		 "manifest_schema2_digest":"sha256:tek","tags":[{"name":"9.8"}]}]}],"total":1}`

	rh, _ := rhServer(t, body)
	ctx := context.Background()
	if _, err := rh.Tags("ubi9/ubi").Next(ctx); err != nil {
		t.Fatalf("Next: %v", err)
	}
	info, err := rh.Inspect(ctx, "ubi9/ubi", "9.8")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if info.Digest != "sha256:tek" {
		t.Errorf("digest = %q, want sha256:tek", info.Digest)
	}
	// No added_date on the tag: the build date stands in.
	if info.LastUpdated != "2026-01-01T00:00:00Z" {
		t.Errorf("last updated = %q, want the build date", info.LastUpdated)
	}
}

// A full page means there may be more; the walk stops once total is reached.
func TestRedHatPagesUntilTotal(t *testing.T) {
	var first, second []string
	for i := 0; i < redHatPageSize; i++ {
		first = append(first, build("amd64", "2026-09-22T09:45:00Z", "ubi9/ubi", "sha256:aa", fmt.Sprintf("9.%d", i)))
		second = append(second, build("amd64", "2026-08-01T00:00:00Z", "ubi9/ubi", "sha256:bb", fmt.Sprintf("8.%d", i)))
	}
	total := 2 * redHatPageSize

	rh, queries := rhServer(t, page(total, first...), page(total, second...))
	ctx := context.Background()
	pager := rh.Tags("ubi9/ubi")

	p1, err := pager.Next(ctx)
	if err != nil || len(p1) != redHatPageSize {
		t.Fatalf("first page = %d tags, %v", len(p1), err)
	}
	p2, err := pager.Next(ctx)
	if err != nil || len(p2) != redHatPageSize {
		t.Fatalf("second page = %d tags, %v", len(p2), err)
	}
	if done, err := pager.Next(ctx); err != nil || done != nil {
		t.Fatalf("third call = (%v, %v), want (nil, nil)", done, err)
	}
	if len(*queries) != 2 {
		t.Fatalf("%d requests, want 2", len(*queries))
	}
	if got := (*queries)[1].Get("page"); got != "1" {
		t.Errorf("second request asked for page %q, want 1", got)
	}
}

// With the architecture filter on, every record is the same architecture, so a
// tag seen again on a later page says nothing new and is not re-emitted.
func TestRedHatDoesNotRepeatATag(t *testing.T) {
	var full []string
	for i := 0; i < redHatPageSize; i++ {
		full = append(full, build("amd64", "2026-09-22T09:45:00Z", "ubi9/ubi", "sha256:aa", fmt.Sprintf("9.%d", i)))
	}
	repeat := build("amd64", "2026-08-01T00:00:00Z", "ubi9/ubi", "sha256:aa", "9.0")

	rh, _ := rhServer(t, page(2*redHatPageSize, full...), page(2*redHatPageSize, repeat))
	ctx := context.Background()
	pager := rh.Tags("ubi9/ubi")

	if _, err := pager.Next(ctx); err != nil {
		t.Fatalf("first page: %v", err)
	}
	again, err := pager.Next(ctx)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second page = %v, want nothing new", again)
	}
}

// End to end: the newest version wins, and no lookup costs a request.
func TestRedHatResolvesNewest(t *testing.T) {
	rh, queries := rhServer(t, page(3,
		build("amd64", "2026-09-22T09:45:00Z", "ubi9/ubi", "sha256:aa", "9.8", "1790067973"),
		build("amd64", "2026-05-01T00:00:00Z", "ubi9/ubi", "sha256:bb", "9.7"),
		build("amd64", "2026-01-01T00:00:00Z", "ubi9/ubi", "sha256:cc", "9.6"),
	))

	info, st, err := resolve(context.Background(), rh, "ubi9/ubi",
		regexp.MustCompile(`^9\.[0-9]+$`), "amd64", "linux")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if info.Tag != "9.8" {
		t.Errorf("tag = %q, want 9.8", info.Tag)
	}
	if st.Pages != 1 {
		t.Errorf("pages = %d, want 1", st.Pages)
	}
	if len(*queries) != 1 {
		t.Errorf("%d requests, want 1: Inspect must not hit the network", len(*queries))
	}
}

// A bare ten-digit build number is not a version, and must not win.
func TestRedHatSkipsBuildNumbers(t *testing.T) {
	rh, _ := rhServer(t, page(1,
		build("amd64", "2026-09-22T09:45:00Z", "ubi9/ubi", "sha256:aa", "1790067973", "9.8"),
	))

	info, _, err := resolve(context.Background(), rh, "ubi9/ubi",
		regexp.MustCompile(""), "amd64", "linux")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if info.Tag != "9.8" {
		t.Errorf("tag = %q, want 9.8", info.Tag)
	}
}

// The catalogue reports errors as RFC 7807 documents; "detail" is the sentence
// worth showing.
func TestRedHatReportsTheApiMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{
			"type": "about:blank", "title": "Bad Request",
			"detail": "Unknown field x in include parameter", "status": 400,
		})
	}))
	defer srv.Close()

	rh := newRedHat("amd64")
	rh.baseURL = srv.URL

	_, err := rh.Tags("ubi9/ubi").Next(context.Background())
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "Unknown field x") {
		t.Errorf("error = %v, want the catalogue's detail", err)
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error = %v, want the status code", err)
	}
}

// registry.redhat.io is not in the catalogue, so it must keep the Distribution
// driver; registry.access.redhat.com must get this one.
func TestPickRegistryChoosesTheCatalogue(t *testing.T) {
	cfg.Architecture = "amd64"

	if _, ok := pickRegistry(Reference{Host: redHatHost, Repo: "ubi9/ubi"}).(*redHat); !ok {
		t.Errorf("%s did not get the catalogue driver", redHatHost)
	}
	if _, ok := pickRegistry(Reference{Host: "registry.redhat.io", Repo: "ubi9/ubi"}).(*distribution); !ok {
		t.Errorf("registry.redhat.io did not get the Distribution driver")
	}
}
