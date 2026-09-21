package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"testing"
)

// fakeRegistry drives the resolve pipeline without a network. It records how
// many pages were read and which tags were inspected, which is how the
// pipeline's frugality (asking for a platform only for names that passed the
// filter) can be asserted.
type fakeRegistry struct {
	pages       [][]string
	info        map[string]TagInfo
	newestFirst bool

	pagesRead int
	inspected []string
}

func (f *fakeRegistry) Name() string      { return "fake" }
func (f *fakeRegistry) NewestFirst() bool { return f.newestFirst }

func (f *fakeRegistry) Tags(string) TagPager { return &fakePager{reg: f} }

func (f *fakeRegistry) Inspect(_ context.Context, _, tag string) (TagInfo, error) {
	f.inspected = append(f.inspected, tag)
	info, ok := f.info[tag]
	if !ok {
		return TagInfo{}, fmt.Errorf("unknown tag %q", tag)
	}
	return info, nil
}

type fakePager struct {
	reg *fakeRegistry
	i   int
}

func (p *fakePager) Next(context.Context) ([]string, error) {
	if p.i >= len(p.reg.pages) {
		return nil, nil
	}
	page := p.reg.pages[p.i]
	p.i++
	p.reg.pagesRead++
	return page, nil
}

// linuxTag builds a single-platform image record.
func linuxTag(tag, arch string) TagInfo {
	return TagInfo{
		Tag:       tag,
		Platforms: []Platform{{Arch: arch, OS: "linux"}},
	}
}

func TestPlatformMatches(t *testing.T) {
	tests := []struct {
		name string
		info TagInfo
		arch string
		want bool
	}{
		{"architecture matches", linuxTag("1.0.0", "amd64"), "amd64", true},
		{"architecture differs", linuxTag("1.0.0", "arm64"), "amd64", false},
		{"no platform information", TagInfo{Tag: "1.0.0"}, "amd64", false},
		{"a plugin passes every platform", TagInfo{Tag: "1.0.0", AnyPlatform: true}, "amd64", true},
		{
			"multi platform",
			TagInfo{Tag: "1.0.0", Platforms: []Platform{
				{Arch: "arm64", OS: "linux"},
				{Arch: "amd64", OS: "linux"},
			}},
			"amd64",
			true,
		},
		{"os differs", TagInfo{Tag: "1.0.0", Platforms: []Platform{{Arch: "amd64", OS: "windows"}}}, "amd64", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := platformMatches(tc.info, tc.arch, "linux"); got != tc.want {
				t.Errorf("platformMatches = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestExcludeRe(t *testing.T) {
	excluded := []string{
		"latest", "latest-alpine", "1.0.0-beta", "1.0.0-beta1", "2.1-rc",
		"3.0.0-rc2", "1.0.0.dev0", "1.0-alpha", "edge", "v2-nightly",
		"1.0.0-SNAPSHOT", "1.0.0-PRE", "1.0.0-preview3",
		// Red Hat source containers.
		"1780376659-source", "9.0.0-1468-source", "1.0.0-source",
		"9.0.0-1468.1655190709-source",
	}
	kept := []string{
		"1.0.0", "1.25.1", "11.6.6-security-01", "1.22-alpine", "torch-1.0",
		"2.0-arch64", "sourcegraph-1.0", "1.0.0-debian-12", "24.04", "3.19.1",
		"1.0.0-devel", "latestish", "9.0.0-1468.1655190709",
	}

	for _, tag := range excluded {
		if !excludeRe.MatchString(tag) {
			t.Errorf("%q should have been excluded but passed", tag)
		}
	}
	for _, tag := range kept {
		if excludeRe.MatchString(tag) {
			t.Errorf("%q should have passed but was excluded", tag)
		}
	}
}

func TestSortTags(t *testing.T) {
	tags := []TagInfo{
		{Tag: "1.0.0"},
		{Tag: "1.1.0"},
		{Tag: "1.1.0-security-01"},
		{Tag: "0.9.0"},
	}

	sortTags(tags)

	want := []string{"1.1.0-security-01", "1.1.0", "1.0.0", "0.9.0"}
	for i, w := range want {
		if tags[i].Tag != w {
			t.Errorf("order[%d] = %s, want %s", i, tags[i].Tag, w)
		}
	}
}

func TestResolveFiltersBeforeInspect(t *testing.T) {
	reg := &fakeRegistry{
		pages:       [][]string{{"1.0.0", "1.1.0-beta", "2.0.0"}},
		newestFirst: true,
		info: map[string]TagInfo{
			"1.0.0":      linuxTag("1.0.0", "amd64"),
			"1.1.0-beta": linuxTag("1.1.0-beta", "amd64"),
			"2.0.0":      linuxTag("2.0.0", "arm64"),
		},
	}

	got, err := resolve(context.Background(), reg, "x/y", regexp.MustCompile(`.*`), "amd64", "linux")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Tag != "1.0.0" {
		t.Errorf("tag = %s, want 1.0.0", got.Tag)
	}

	// An excluded tag must not be inspected: this is what keeps the request
	// count down to the number of candidates on expensive drivers.
	if slices.Contains(reg.inspected, "1.1.0-beta") {
		t.Errorf("an excluded tag was inspected: %v", reg.inspected)
	}
}

func TestResolveMatchesRequestedArch(t *testing.T) {
	newReg := func() *fakeRegistry {
		return &fakeRegistry{
			pages:       [][]string{{"1.0.0", "2.0.0"}},
			newestFirst: true,
			info: map[string]TagInfo{
				"1.0.0": linuxTag("1.0.0", "amd64"),
				"2.0.0": linuxTag("2.0.0", "arm64"),
			},
		}
	}

	got, err := resolve(context.Background(), newReg(), "x/y", regexp.MustCompile(`.*`), "arm64", "linux")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Tag != "2.0.0" {
		t.Errorf("tag = %s, want 2.0.0", got.Tag)
	}
}

func TestResolveStopsPagingWhenNewestFirst(t *testing.T) {
	pages := [][]string{{"1.0.0"}, {"9.9.9"}}
	info := map[string]TagInfo{
		"1.0.0": linuxTag("1.0.0", "amd64"),
		"9.9.9": linuxTag("9.9.9", "amd64"),
	}

	// When pages arrive newest-first, paging stops at the first match.
	first := &fakeRegistry{pages: pages, info: info, newestFirst: true}
	got, err := resolve(context.Background(), first, "x/y", regexp.MustCompile(`.*`), "amd64", "linux")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Tag != "1.0.0" {
		t.Errorf("tag = %s, want 1.0.0", got.Tag)
	}
	if first.pagesRead != 1 {
		t.Errorf("pages read = %d, want 1", first.pagesRead)
	}

	// Without an ordering guarantee (OCI Distribution) every page is scanned.
	all := &fakeRegistry{pages: pages, info: info, newestFirst: false}
	got, err = resolve(context.Background(), all, "x/y", regexp.MustCompile(`.*`), "amd64", "linux")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Tag != "9.9.9" {
		t.Errorf("tag = %s, want 9.9.9", got.Tag)
	}
	// Two pages are read; the nil call that signals the end is not counted.
	if all.pagesRead != 2 {
		t.Errorf("pages read = %d, want 2", all.pagesRead)
	}
}

// TestResolvePrefersLongerPrefix documents the existing "prefix plus longer"
// heuristic: semver puts a prerelease below the plain release, so the order is
// 1.2.3 then 1.2.3-alpine and the rule picks the variant. The behaviour is
// kept deliberately.
func TestResolvePrefersLongerPrefix(t *testing.T) {
	reg := &fakeRegistry{
		pages:       [][]string{{"1.2.3", "1.2.3-alpine"}},
		newestFirst: true,
		info: map[string]TagInfo{
			"1.2.3":        linuxTag("1.2.3", "amd64"),
			"1.2.3-alpine": linuxTag("1.2.3-alpine", "amd64"),
		},
	}

	got, err := resolve(context.Background(), reg, "x/y", regexp.MustCompile(`.*`), "amd64", "linux")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Tag != "1.2.3-alpine" {
		t.Errorf("tag = %s, want 1.2.3-alpine", got.Tag)
	}
}

func TestResolveNoMatch(t *testing.T) {
	reg := &fakeRegistry{
		pages:       [][]string{{"1.0.0"}},
		newestFirst: true,
		info:        map[string]TagInfo{"1.0.0": linuxTag("1.0.0", "amd64")},
	}

	_, err := resolve(context.Background(), reg, "x/y", regexp.MustCompile(`^yok$`), "amd64", "linux")
	if !errors.Is(err, ErrNoMatch) {
		t.Errorf("error = %v, want ErrNoMatch", err)
	}
}
