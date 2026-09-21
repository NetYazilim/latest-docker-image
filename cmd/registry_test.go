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
	// prefixes records what each Tags call was asked to skip to.
	prefixes []string
	// emptyForPrefix makes a skipped listing come back empty, standing in for a
	// registry whose ordering the skip cannot rely on.
	emptyForPrefix bool
}

func (f *fakeRegistry) Name() string      { return "fake" }
func (f *fakeRegistry) NewestFirst() bool { return f.newestFirst }

func (f *fakeRegistry) Tags(_, prefix string) TagPager {
	f.prefixes = append(f.prefixes, prefix)
	if prefix != "" && f.emptyForPrefix {
		return &fakePager{reg: f, empty: true}
	}
	return &fakePager{reg: f}
}

func (f *fakeRegistry) Inspect(_ context.Context, _, tag string) (TagInfo, error) {
	f.inspected = append(f.inspected, tag)
	info, ok := f.info[tag]
	if !ok {
		return TagInfo{}, fmt.Errorf("unknown tag %q", tag)
	}
	return info, nil
}

type fakePager struct {
	reg   *fakeRegistry
	i     int
	empty bool
}

func (p *fakePager) Next(context.Context) ([]string, error) {
	if p.empty {
		return nil, nil
	}
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

func TestResolveStopsPagingWhenNewestFirst(t *testing.T) {
	pages := [][]string{{"1.0.0"}, {"9.9.9"}}
	info := map[string]TagInfo{
		"1.0.0": linuxTag("1.0.0", "amd64"),
		"9.9.9": linuxTag("9.9.9", "amd64"),
	}

	// When pages arrive newest-first, paging stops at the first match.
	first := &fakeRegistry{pages: pages, info: info, newestFirst: true}
	got, _, err := resolve(context.Background(), first, "x/y", regexp.MustCompile(`.*`), "amd64", "linux")
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

func TestAnchoredPrefix(t *testing.T) {
	tests := []struct {
		pattern string
		want    string
	}{
		{`^22\.2026\.09\.(\d+)\.(\d+)$`, "22.2026.09."},
		{`^9\.[0-9]+$`, "9."},
		{`^v(\d+)\.(\d+)\.(\d+)$`, "v"},
		{`^latest$`, "latest"},
		// An unescaped dot is a metacharacter, so the prefix stops before it.
		// That costs precision, and now it costs speed too.
		{`^22.2026.09\.(\d+)$`, "22"},
		// Unanchored: a literal can match anywhere in the tag, so it says
		// nothing about where the tag starts.
		{`(\d+)\.(\d+)\.(\d+)$`, ""},
		{`-alpine$`, ""},
		{`22\.2026`, ""},
		{"", ""},
		{`^(\d+)\.(\d+)$`, ""},
	}

	for _, tc := range tests {
		if got := anchoredPrefix(regexp.MustCompile(tc.pattern)); got != tc.want {
			t.Errorf("anchoredPrefix(%q) = %q, want %q", tc.pattern, got, tc.want)
		}
	}
}

// TestResolvePassesPrefixToDriver: an anchored filter has to reach the driver,
// which is what lets it read a slice of the tag list instead of all of it.
func TestResolvePassesPrefixToDriver(t *testing.T) {
	reg := &fakeRegistry{
		pages:       [][]string{{"9.8"}},
		newestFirst: false,
		info:        map[string]TagInfo{"9.8": linuxTag("9.8", "amd64")},
	}

	if _, _, err := resolve(context.Background(), reg, "ubi9/ubi", regexp.MustCompile(`^9\.[0-9]+$`), "amd64", "linux"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := []string{"9."}; !slices.Equal(reg.prefixes, want) {
		t.Errorf("prefixes = %v, want %v", reg.prefixes, want)
	}
}

// TestResolveRereadsWithoutPrefix is the safety net. A registry that honours
// last= but not the ordering it implies would hide matching tags behind the
// skip; finding nothing is the signal to read the list in full. Worst case the
// work is done twice, and only in a case that was going to fail anyway.
func TestResolveRereadsWithoutPrefix(t *testing.T) {
	reg := &fakeRegistry{
		pages:          [][]string{{"9.8", "9.6"}},
		newestFirst:    false,
		emptyForPrefix: true,
		info: map[string]TagInfo{
			"9.8": linuxTag("9.8", "amd64"),
			"9.6": linuxTag("9.6", "amd64"),
		},
	}

	got, st, err := resolve(context.Background(), reg, "ubi9/ubi", regexp.MustCompile(`^9\.[0-9]+$`), "amd64", "linux")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Tag != "9.8" {
		t.Errorf("tag = %s, want 9.8 - the second pass should have found it", got.Tag)
	}
	if want := []string{"9.", ""}; !slices.Equal(reg.prefixes, want) {
		t.Errorf("prefixes = %v, want %v (skip, then the full list)", reg.prefixes, want)
	}
	// The reported cost has to cover both passes, or -verbose would understate
	// the work.
	if st.Pages != 1 || st.Tags != 2 {
		t.Errorf("stats = %+v, want the pages and tags of both passes", st)
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
