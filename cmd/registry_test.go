package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
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
		// cosign artifacts, which make up nearly all of gcr.io/distroless.
		"sha256-2f1c5e0a.sig", "sha256-2f1c5e0a.att", "sha256-2f1c5e0a.sbom",
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

func TestCompareTags(t *testing.T) {
	// Each pair is {higher, lower}: the first must sort before the second.
	pairs := [][2]string{
		// Numeric, not lexical.
		{"1.10.0", "1.9.0"},
		{"9.8", "9.6"},
		{"v2.63.23", "v2.63.9"},
		// A leading zero in a segment, which semver rejects outright.
		{"26.04", "24.04"},
		{"24.04", "22.04"},
		// More than three segments, as public.ecr.aws/lambda/nodejs publishes.
		{"22.2025.04.24.11", "22.2024.12.01.03"},
		{"22.2026.01.02.03", "22.2025.04.24.11"},
		{"22.2025.04.24.11", "22.2025.04.24.09"},
		// A plain release outranks a suffixed build of the same version.
		{"1.2.3", "1.2.3-alpine"},
		{"3.7.8", "3.7.8-amd64"},
		// Except a security rebuild, which supersedes it.
		{"11.6.6-security-01", "11.6.6"},
		{"11.6.6-security-02", "11.6.6-security-01"},
		// A higher version wins even when the lower one is plain.
		{"1.26.0-alpine", "1.25.1"},
	}

	for _, p := range pairs {
		if c := compareTags(p[0], p[1]); c >= 0 {
			t.Errorf("compareTags(%q, %q) = %d, want negative", p[0], p[1], c)
		}
		if c := compareTags(p[1], p[0]); c <= 0 {
			t.Errorf("compareTags(%q, %q) = %d, want positive", p[1], p[0], c)
		}
	}

	// 1.2 and 1.2.0 are the same version; a missing segment counts as zero.
	for _, same := range [][2]string{{"1.2", "1.2.0"}, {"v1.2.0", "1.2"}, {"1.0.0", "1"}} {
		if c := compareTags(same[0], same[1]); c != 0 {
			t.Errorf("compareTags(%q, %q) = %d, want 0", same[0], same[1], c)
		}
	}
}

func TestSplitTag(t *testing.T) {
	tests := []struct {
		tag    string
		core   []int
		suffix string
	}{
		{"1.37.1", []int{1, 37, 1}, ""},
		{"v1.37.1-alpine", []int{1, 37, 1}, "-alpine"},
		{"24.04", []int{24, 4}, ""},
		{"22.2025.04.24.11", []int{22, 2025, 4, 24, 11}, ""},
		{"9.2-696", []int{9, 2}, "-696"},
		{"1.0.0+build.5", []int{1, 0, 0}, "+build.5"},
		// Not version-shaped: no core, so such tags keep their order.
		{"latest", nil, ""},
		{"0093a0209f695c939427fd207c933bdbadcf7301", nil, ""},
		{"nonroot", nil, ""},
	}

	for _, tc := range tests {
		core, suffix := splitTag(tc.tag)
		if !slices.Equal(core, tc.core) {
			t.Errorf("splitTag(%q) core = %v, want %v", tc.tag, core, tc.core)
		}
		if suffix != tc.suffix {
			t.Errorf("splitTag(%q) suffix = %q, want %q", tc.tag, suffix, tc.suffix)
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

	got, _, err := resolve(context.Background(), reg, "x/y", regexp.MustCompile(`.*`), "amd64", "linux")
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

	got, _, err := resolve(context.Background(), newReg(), "x/y", regexp.MustCompile(`.*`), "arm64", "linux")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Tag != "2.0.0" {
		t.Errorf("tag = %s, want 2.0.0", got.Tag)
	}
}

func TestResolveReadsOnPastTheFirstMatch(t *testing.T) {
	pages := [][]string{{"1.0.0"}, {"9.9.9"}}
	info := map[string]TagInfo{
		"1.0.0": linuxTag("1.0.0", "amd64"),
		"9.9.9": linuxTag("9.9.9", "amd64"),
	}

	// Newest-first pages are ordered by date, not by version, so the page
	// that matched is not the end of it: the walk goes on while each page
	// improves on the best version so far. This used to stop at the first
	// match and answer 1.0.0, which is the defect the rule was added for.
	first := &fakeRegistry{pages: pages, info: info, newestFirst: true}
	got, _, err := resolve(context.Background(), first, "x/y", regexp.MustCompile(`.*`), "amd64", "linux")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Tag != "9.9.9" {
		t.Errorf("tag = %s, want 9.9.9", got.Tag)
	}
	if first.pagesRead != 2 {
		t.Errorf("pages read = %d, want 2", first.pagesRead)
	}

	// Without an ordering guarantee (OCI Distribution) every page is scanned.
	all := &fakeRegistry{pages: pages, info: info, newestFirst: false}
	got, _, err = resolve(context.Background(), all, "x/y", regexp.MustCompile(`.*`), "amd64", "linux")
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

// TestResolvePrefersPlainRelease pins the behaviour that replaced the old
// "prefix plus longer" heuristic. That rule preferred a suffixed tag over the
// release it was built from, which is how `ldi grafana/loki` came back with
// 3.7.8-amd64 - a single-architecture image - instead of 3.7.8.
func TestResolvePrefersPlainRelease(t *testing.T) {
	for _, variant := range []string{"1.2.3-alpine", "1.2.3-amd64"} {
		reg := &fakeRegistry{
			pages:       [][]string{{"1.2.3", variant}},
			newestFirst: true,
			info: map[string]TagInfo{
				"1.2.3": linuxTag("1.2.3", "amd64"),
				variant: linuxTag(variant, "amd64"),
			},
		}

		got, _, err := resolve(context.Background(), reg, "x/y", regexp.MustCompile(`.*`), "amd64", "linux")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Tag != "1.2.3" {
			t.Errorf("with %s present, tag = %s, want 1.2.3", variant, got.Tag)
		}
	}
}

// TestResolveStopsAtFirstMatch covers the lazy path taken by a driver that
// cannot order its tags: the candidates are looked up from the top of the
// sorted list and the walk stops at the first one published for the requested
// platform. The names are already in descending order, so the assertion holds
// whether or not the sort reorders them.
func TestResolveStopsAtFirstMatch(t *testing.T) {
	reg := &fakeRegistry{
		pages:       [][]string{{"3.0.0", "2.0.0", "1.0.0"}},
		newestFirst: false,
		info: map[string]TagInfo{
			"3.0.0": linuxTag("3.0.0", "amd64"),
			"2.0.0": linuxTag("2.0.0", "amd64"),
			"1.0.0": linuxTag("1.0.0", "amd64"),
		},
	}

	if _, _, err := resolve(context.Background(), reg, "x/y", regexp.MustCompile(`.*`), "amd64", "linux"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(reg.inspected) != 1 {
		t.Errorf("inspected %v, want exactly one lookup", reg.inspected)
	}
	for _, unwanted := range []string{"2.0.0", "1.0.0"} {
		if slices.Contains(reg.inspected, unwanted) {
			t.Errorf("%s should never have been looked up: %v", unwanted, reg.inspected)
		}
	}
}

// TestResolveInspectsHighestFirst pins the order of those lookups: highest
// version first, so the answer is usually the first request.
func TestResolveInspectsHighestFirst(t *testing.T) {
	reg := &fakeRegistry{
		pages:       [][]string{{"1.0.0", "3.0.0", "2.0.0"}},
		newestFirst: false,
		info: map[string]TagInfo{
			"1.0.0": linuxTag("1.0.0", "amd64"),
			"2.0.0": linuxTag("2.0.0", "amd64"),
			"3.0.0": linuxTag("3.0.0", "amd64"),
		},
	}

	got, _, err := resolve(context.Background(), reg, "x/y", regexp.MustCompile(`.*`), "amd64", "linux")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Tag != "3.0.0" {
		t.Errorf("tag = %s, want 3.0.0", got.Tag)
	}
	if want := []string{"3.0.0"}; !slices.Equal(reg.inspected, want) {
		t.Errorf("inspected %v, want %v", reg.inspected, want)
	}
}

// TestResolveCapsInspection guards against the gcr.io/distroless case: a
// repository whose tag list is enormous and whose candidates do not match the
// requested platform must fail fast with advice, not issue a request per tag.
func TestResolveCapsInspection(t *testing.T) {
	var names []string
	info := map[string]TagInfo{}
	for i := 0; i < maxInspect+10; i++ {
		name := fmt.Sprintf("1.0.%d", i)
		names = append(names, name)
		// Published, but never for the platform being asked about.
		info[name] = linuxTag(name, "s390x")
	}

	reg := &fakeRegistry{pages: [][]string{names}, newestFirst: false, info: info}

	_, _, err := resolve(context.Background(), reg, "x/y", regexp.MustCompile(`.*`), "amd64", "linux")
	if err == nil {
		t.Fatal("expected an error once the lookup cap was reached")
	}
	if !strings.Contains(err.Error(), "narrow the tag filter") {
		t.Errorf("error = %q, should tell the user what to do", err)
	}
	if len(reg.inspected) != maxInspect {
		t.Errorf("made %d lookups, want the cap of %d", len(reg.inspected), maxInspect)
	}
}

func TestIsVersionLike(t *testing.T) {
	versions := []string{
		"1.0.0", "9.8", "v2.63.23", "1.37.1-alpine", "11.6.6-security-01",
		"24.04", "9.0.0-1468.1655190709", "3.7.8", "2.45.1-alpine",
		// Bare major versions are real: node:22, python:3, ubuntu:24.
		"3", "22", "24", "2024", "v22",
	}
	buildIDs := []string{
		// Epoch stamps and dates, which semver would read as enormous majors.
		"1789646103", "1780376659", "20250101", "1788245146",
		// Commit hashes, as gcr.io/distroless tags its images.
		"0093a0209f695c939427fd207c933bdbadcf7301",
		// Names, not versions.
		"nonroot", "debug", "base-debian10", "stable",
	}

	for _, tag := range versions {
		if !isVersionLike(tag) {
			t.Errorf("%q should read as a version", tag)
		}
	}
	for _, tag := range buildIDs {
		if isVersionLike(tag) {
			t.Errorf("%q should not read as a version", tag)
		}
	}
}

// TestResolveRejectsBuildIdentifiers is the ubi9/ubi case: an epoch tag sits
// beside the real releases and semver would rank it above all of them.
func TestResolveRejectsBuildIdentifiers(t *testing.T) {
	reg := &fakeRegistry{
		pages:       [][]string{{"1789646103", "9.6", "9.8"}},
		newestFirst: false,
		info: map[string]TagInfo{
			"1789646103": linuxTag("1789646103", "amd64"),
			"9.6":        linuxTag("9.6", "amd64"),
			"9.8":        linuxTag("9.8", "amd64"),
		},
	}

	got, _, err := resolve(context.Background(), reg, "ubi9/ubi", regexp.MustCompile(`.*`), "amd64", "linux")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Tag == "1789646103" {
		t.Error("an epoch stamp was returned as the latest version")
	}
	if slices.Contains(reg.inspected, "1789646103") {
		t.Errorf("the epoch stamp should not even be looked up: %v", reg.inspected)
	}
}

// TestResolveNoVersionLikeTag is the gcr.io/distroless case: every tag is a
// commit hash, so there is no latest version to report.
func TestResolveNoVersionLikeTag(t *testing.T) {
	const hash = "0093a0209f695c939427fd207c933bdbadcf7301"

	reg := &fakeRegistry{
		pages:       [][]string{{hash, "1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b"}},
		newestFirst: false,
		info:        map[string]TagInfo{},
	}

	_, _, err := resolve(context.Background(), reg, "distroless/base", regexp.MustCompile(`.*`), "amd64", "linux")
	if !errors.Is(err, ErrNoVersion) {
		t.Fatalf("error = %v, want ErrNoVersion", err)
	}
	// The message has to show what the tags look like, or the advice to add a
	// filter is unusable.
	if !strings.Contains(err.Error(), hash) {
		t.Errorf("error = %q, should quote an example tag", err)
	}
	if len(reg.inspected) != 0 {
		t.Errorf("nothing should be looked up: %v", reg.inspected)
	}
}

// TestSortTagsBreaksTiesByDate covers the Docker Hub tie-break: "1.2.3" and
// "v1.2.3" are the same version under semver, so the more recently updated one
// wins. Distribution leaves the date empty and those keep their order.
func TestSortTagsBreaksTiesByDate(t *testing.T) {
	tags := []TagInfo{
		{Tag: "1.2.3", LastUpdated: "2024-01-02T03:04:05Z"},
		{Tag: "v1.2.3", LastUpdated: "2025-06-07T08:09:10Z"},
	}

	sortTags(tags)

	if tags[0].Tag != "v1.2.3" {
		t.Errorf("order = %s then %s, want the newer v1.2.3 first", tags[0].Tag, tags[1].Tag)
	}

	// With no dates at all the order must simply be stable.
	undated := []TagInfo{{Tag: "1.2.3"}, {Tag: "v1.2.3"}}
	sortTags(undated)
	if undated[0].Tag != "1.2.3" {
		t.Errorf("undated order = %s, want the input order preserved", undated[0].Tag)
	}
}

func TestNamedTag(t *testing.T) {
	// A filter that names one tag: the exclusion rules must step aside.
	explicit := []string{"latest", "^latest$", "stable", "^stable$", "nonroot", `^1\.2\.3$`}
	// A filter that selects among tags, or no filter at all: the rules apply.
	general := []string{
		"", ".*", `(\d+)\.(\d+)\.(\d+)`, `^(\d+)\.(\d+)\.(\d+)$`,
		`-alpine$`, `^v(\d+)\.(\d+)\.(\d+)$`, `^9\.[0-9]+$`,
	}

	for _, p := range explicit {
		if _, ok := namedTag(regexp.MustCompile(p)); !ok {
			t.Errorf("%q names one tag outright", p)
		}
	}
	for _, p := range general {
		if name, ok := namedTag(regexp.MustCompile(p)); ok {
			t.Errorf("%q selects among tags and must keep the rules, got %q", p, name)
		}
	}

	// The name has to come back unadorned, since it is compared for equality.
	if name, _ := namedTag(regexp.MustCompile(`^latest$`)); name != "latest" {
		t.Errorf("name = %q, want latest", name)
	}
}

// TestResolveHonoursExplicitTag is the reported bug: latest is on the exclusion
// list, so asking for it by name answered "not found" for a tag that exists.
func TestResolveHonoursExplicitTag(t *testing.T) {
	// latest-amd64 is here on purpose: a bare "latest" filter is an unanchored
	// regex, so matching with it used to accept the variant, and the prefix
	// rule then preferred it. grafana/loki answered latest-amd64 that way.
	reg := &fakeRegistry{
		pages:       [][]string{{"latest", "latest-amd64", "latest-arm64", "1.0.0"}},
		newestFirst: false,
		info: map[string]TagInfo{
			"latest":       linuxTag("latest", "amd64"),
			"latest-amd64": linuxTag("latest-amd64", "amd64"),
			"latest-arm64": linuxTag("latest-arm64", "amd64"),
			"1.0.0":        linuxTag("1.0.0", "amd64"),
		},
	}

	for _, pattern := range []string{"latest", "^latest$"} {
		reg.inspected = nil

		got, _, err := resolve(context.Background(), reg, "x/y", regexp.MustCompile(pattern), "amd64", "linux")
		if err != nil {
			t.Fatalf("%q: unexpected error: %v", pattern, err)
		}
		if got.Tag != "latest" {
			t.Errorf("%q: tag = %s, want latest", pattern, got.Tag)
		}
		if len(reg.inspected) != 1 {
			t.Errorf("%q: looked up %v, want only the named tag", pattern, reg.inspected)
		}
	}
}

// TestResolveHonoursExplicitNonVersionTag: naming a tag also waives the
// version-like requirement, since the user was not asking "which is newest".
func TestResolveHonoursExplicitNonVersionTag(t *testing.T) {
	reg := &fakeRegistry{
		pages:       [][]string{{"nonroot", "debug"}},
		newestFirst: false,
		info:        map[string]TagInfo{"nonroot": linuxTag("nonroot", "amd64")},
	}

	got, _, err := resolve(context.Background(), reg, "distroless/base", regexp.MustCompile(`^nonroot$`), "amd64", "linux")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Tag != "nonroot" {
		t.Errorf("tag = %s, want nonroot", got.Tag)
	}
}

// TestResolveReportsExcludedTags: when a general filter matched only excluded
// tags, say so instead of claiming nothing was found.
func TestResolveReportsExcludedTags(t *testing.T) {
	reg := &fakeRegistry{
		pages:       [][]string{{"1.0.0-rc1", "1.0.0-rc2"}},
		newestFirst: false,
		info:        map[string]TagInfo{},
	}

	_, _, err := resolve(context.Background(), reg, "x/y", regexp.MustCompile(`-rc\d$`), "amd64", "linux")
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, ErrNoMatch) {
		t.Error(`this is not "not found": the tags matched and were then excluded`)
	}
	for _, want := range []string{"excluded", "1.0.0-rc1", "name the tag exactly"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, should contain %q", err, want)
		}
	}
}

// TestResolveReportsStats covers the numbers behind -verbose. They exist to
// answer "why is this slow", so they have to be right: the lookup count in
// particular is one request per tag on a Distribution registry.
func TestResolveReportsStats(t *testing.T) {
	reg := &fakeRegistry{
		pages: [][]string{
			{"3.0.0", "2.0.0", "latest"},
			{"1.0.0", "nonroot"},
		},
		newestFirst: false,
		info: map[string]TagInfo{
			"3.0.0": linuxTag("3.0.0", "amd64"),
			"2.0.0": linuxTag("2.0.0", "amd64"),
			"1.0.0": linuxTag("1.0.0", "amd64"),
		},
	}

	_, st, err := resolve(context.Background(), reg, "x/y", regexp.MustCompile(`.*`), "amd64", "linux")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Two pages, five tags; latest is excluded and nonroot is not a version, so
	// three candidates remain; the walk stops at the first match.
	want := lookupStats{Pages: 2, Tags: 5, Candidates: 3, Lookups: 1}
	if st != want {
		t.Errorf("stats = %+v, want %+v", st, want)
	}
}

// TestResolveStopsOnceNamedTagFound: an exact name can match one tag, so the
// rest of the list is not worth reading. Without this a named lookup on a large
// repository read everything after the tag as well.
func TestResolveStopsOnceNamedTagFound(t *testing.T) {
	reg := &fakeRegistry{
		pages:       [][]string{{"latest"}, {"nonroot"}, {"zzz"}},
		newestFirst: false,
		info:        map[string]TagInfo{"latest": linuxTag("latest", "amd64")},
	}

	got, st, err := resolve(context.Background(), reg, "x/y", regexp.MustCompile(`^latest$`), "amd64", "linux")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Tag != "latest" {
		t.Errorf("tag = %s, want latest", got.Tag)
	}
	if st.Pages != 1 {
		t.Errorf("read %d pages, want 1: the tag was on the first", st.Pages)
	}
}

func TestResolveNoMatch(t *testing.T) {
	reg := &fakeRegistry{
		pages:       [][]string{{"1.0.0"}},
		newestFirst: true,
		info:        map[string]TagInfo{"1.0.0": linuxTag("1.0.0", "amd64")},
	}

	_, _, err := resolve(context.Background(), reg, "x/y", regexp.MustCompile(`^yok$`), "amd64", "linux")
	if !errors.Is(err, ErrNoMatch) {
		t.Errorf("error = %v, want ErrNoMatch", err)
	}
}

// datedTag builds a single-platform record carrying a date, so the ordered
// path can be given a page whose date order disagrees with version order.
func datedTag(tag, when string) TagInfo {
	return TagInfo{
		Tag:         tag,
		Platforms:   []Platform{{Arch: "amd64", OS: "linux"}},
		LastUpdated: when,
	}
}

// A page that does not improve on the best version so far ends the walk.
func TestOrderedStopsAtThePageThatDoesNotImprove(t *testing.T) {
	reg := &fakeRegistry{
		pages: [][]string{
			{"18.6", "17.9", "16.13"},
			{"15.17", "14.22"},
		},
		newestFirst: true,
		info: map[string]TagInfo{
			"18.6":  datedTag("18.6", "2026-09-19T07:10:36Z"),
			"17.9":  datedTag("17.9", "2026-09-18T00:00:00Z"),
			"16.13": datedTag("16.13", "2026-09-17T00:00:00Z"),
			"15.17": datedTag("15.17", "2026-01-01T00:00:00Z"),
			"14.22": datedTag("14.22", "2025-12-01T00:00:00Z"),
		},
	}

	got, st, err := resolve(context.Background(), reg, "x/y", regexp.MustCompile(``), "amd64", "linux")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Tag != "18.6" {
		t.Errorf("tag = %q, want 18.6", got.Tag)
	}
	if st.Pages != 2 {
		t.Errorf("read %d pages, want 2: one to answer, one to check", st.Pages)
	}
}

// grafana/grafana-oss: the page is ordered by date, and the winning version is
// not the most recently updated tag on it. That is the signal that the
// repository patches older release lines after newer ones, so one more page is
// read - and, finding nothing better, the walk stops there.
func TestOrderedReadsOnWhenTheFirstPageDisagrees(t *testing.T) {
	reg := &fakeRegistry{
		pages: [][]string{
			{"12.4.1", "13.0.2", "12.4.0"},
			{"11.9.3", "11.9.2"},
			{"10.1.0"},
		},
		newestFirst: true,
		info: map[string]TagInfo{
			"12.4.1": datedTag("12.4.1", "2026-09-20T00:00:00Z"),
			"13.0.2": datedTag("13.0.2", "2026-06-02T13:30:00Z"),
			"12.4.0": datedTag("12.4.0", "2026-05-01T00:00:00Z"),
			"11.9.3": datedTag("11.9.3", "2026-04-01T00:00:00Z"),
			"11.9.2": datedTag("11.9.2", "2026-03-01T00:00:00Z"),
			"10.1.0": datedTag("10.1.0", "2026-01-01T00:00:00Z"),
		},
	}

	got, st, err := resolve(context.Background(), reg, "x/y", regexp.MustCompile(``), "amd64", "linux")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Tag != "13.0.2" {
		t.Errorf("tag = %q, want 13.0.2", got.Tag)
	}
	if st.Pages != 2 {
		t.Errorf("read %d pages, want 2: the second did not improve", st.Pages)
	}
}

// The case the rule exists for: the highest version has already fallen off the
// first page, pushed out by newer patches to an older release line. Stopping at
// the first page that matched would have answered 12.4.1.
func TestOrderedFindsAVersionPastTheFirstPage(t *testing.T) {
	reg := &fakeRegistry{
		pages: [][]string{
			{"12.4.1", "12.4.0"},
			{"13.0.2", "12.3.9"},
			{"11.9.3"},
		},
		newestFirst: true,
		info: map[string]TagInfo{
			"12.4.1": datedTag("12.4.1", "2026-09-20T00:00:00Z"),
			"12.4.0": datedTag("12.4.0", "2026-09-01T00:00:00Z"),
			"13.0.2": datedTag("13.0.2", "2026-06-02T13:30:00Z"),
			"12.3.9": datedTag("12.3.9", "2026-05-01T00:00:00Z"),
			"11.9.3": datedTag("11.9.3", "2026-04-01T00:00:00Z"),
		},
	}

	got, st, err := resolve(context.Background(), reg, "x/y", regexp.MustCompile(``), "amd64", "linux")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Tag != "13.0.2" {
		t.Errorf("tag = %q, want 13.0.2 from the second page", got.Tag)
	}
	if st.Pages != 3 {
		t.Errorf("read %d pages, want 3: page 2 improved, page 3 did not", st.Pages)
	}
}

// Naming one tag outright still costs a single page: the page carries only
// that tag, so it is trivially both the newest and the highest.
func TestOrderedNamedTagStillReadsOnePage(t *testing.T) {
	reg := &fakeRegistry{
		pages: [][]string{
			{"latest"},
			{"1.0.0"},
		},
		newestFirst: true,
		info: map[string]TagInfo{
			"latest": datedTag("latest", "2026-09-20T00:00:00Z"),
			"1.0.0":  datedTag("1.0.0", "2026-09-19T00:00:00Z"),
		},
	}

	got, st, err := resolve(context.Background(), reg, "x/y", regexp.MustCompile(`latest`), "amd64", "linux")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Tag != "latest" {
		t.Errorf("tag = %q, want latest", got.Tag)
	}
	if st.Pages != 1 {
		t.Errorf("read %d pages, want 1", st.Pages)
	}
}
