package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"testing"
)

// fakeRegistry, resolve hattını ağ olmadan sürer. Kaç sayfa okunduğunu ve
// hangi tag'lerin Inspect edildiğini kaydeder; hattın ucuz davranması
// (platform sorgusunu yalnız isim filtresini geçenler için yapması) böyle
// doğrulanabiliyor.
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
		return TagInfo{}, fmt.Errorf("bilinmeyen tag %q", tag)
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

// linuxTag, tek platformlu bir imaj kaydı üretir.
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
		{"mimari uyuyor", linuxTag("1.0.0", "amd64"), "amd64", true},
		{"mimari uymuyor", linuxTag("1.0.0", "arm64"), "amd64", false},
		{"platform bilgisi yok", TagInfo{Tag: "1.0.0"}, "amd64", false},
		{"plugin her platformu geçer", TagInfo{Tag: "1.0.0", AnyPlatform: true}, "amd64", true},
		{
			"çok platformlu",
			TagInfo{Tag: "1.0.0", Platforms: []Platform{
				{Arch: "arm64", OS: "linux"},
				{Arch: "amd64", OS: "linux"},
			}},
			"amd64",
			true,
		},
		{"os uymuyor", TagInfo{Tag: "1.0.0", Platforms: []Platform{{Arch: "amd64", OS: "windows"}}}, "amd64", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := platformMatches(tc.info, tc.arch, "linux"); got != tc.want {
				t.Errorf("platformMatches = %v, beklenen %v", got, tc.want)
			}
		})
	}
}

func TestExcludeRe(t *testing.T) {
	elenmeli := []string{
		"latest", "latest-alpine", "1.0.0-beta", "1.0.0-beta1", "2.1-rc",
		"3.0.0-rc2", "1.0.0.dev0", "1.0-alpha", "edge", "v2-nightly",
		"1.0.0-SNAPSHOT", "1.0.0-PRE", "1.0.0-preview3",
	}
	gecmeli := []string{
		"1.0.0", "1.25.1", "11.6.6-security-01", "1.22-alpine", "torch-1.0",
		"2.0-arch64", "sourcegraph-1.0", "1.0.0-debian-12", "24.04", "3.19.1",
		"1.0.0-devel", "latestish", "9.0.0-1468.1655190709",
	}

	for _, tag := range elenmeli {
		if !excludeRe.MatchString(tag) {
			t.Errorf("%q elenmeliydi ama geçti", tag)
		}
	}
	for _, tag := range gecmeli {
		if excludeRe.MatchString(tag) {
			t.Errorf("%q geçmeliydi ama elendi", tag)
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
			t.Errorf("sıra[%d] = %s, beklenen %s", i, tags[i].Tag, w)
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
		t.Fatalf("beklenmeyen hata: %v", err)
	}
	if got.Tag != "1.0.0" {
		t.Errorf("tag = %s, beklenen 1.0.0", got.Tag)
	}

	// Elenen tag için Inspect çağrılmamalı: pahalı sürücülerde istek sayısını
	// aday sayısına indiren şey bu.
	if slices.Contains(reg.inspected, "1.1.0-beta") {
		t.Errorf("elenen tag Inspect edildi: %v", reg.inspected)
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
		t.Fatalf("beklenmeyen hata: %v", err)
	}
	if got.Tag != "2.0.0" {
		t.Errorf("tag = %s, beklenen 2.0.0", got.Tag)
	}
}

func TestResolveStopsPagingWhenNewestFirst(t *testing.T) {
	pages := [][]string{{"1.0.0"}, {"9.9.9"}}
	info := map[string]TagInfo{
		"1.0.0": linuxTag("1.0.0", "amd64"),
		"9.9.9": linuxTag("9.9.9", "amd64"),
	}

	// Sayfalar en yeniden eskiye geliyorsa ilk eşleşmede durulur.
	first := &fakeRegistry{pages: pages, info: info, newestFirst: true}
	got, err := resolve(context.Background(), first, "x/y", regexp.MustCompile(`.*`), "amd64", "linux")
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}
	if got.Tag != "1.0.0" {
		t.Errorf("tag = %s, beklenen 1.0.0", got.Tag)
	}
	if first.pagesRead != 1 {
		t.Errorf("okunan sayfa = %d, beklenen 1", first.pagesRead)
	}

	// Sıralama garantisi yoksa (OCI Distribution) tüm sayfalar taranmalı.
	all := &fakeRegistry{pages: pages, info: info, newestFirst: false}
	got, err = resolve(context.Background(), all, "x/y", regexp.MustCompile(`.*`), "amd64", "linux")
	if err != nil {
		t.Fatalf("beklenmeyen hata: %v", err)
	}
	if got.Tag != "9.9.9" {
		t.Errorf("tag = %s, beklenen 9.9.9", got.Tag)
	}
	if all.pagesRead != 3 {
		t.Errorf("okunan sayfa = %d, beklenen 3 (iki sayfa + bitiş)", all.pagesRead)
	}
}

// TestResolvePrefersLongerPrefix, mevcut "prefix + daha uzun" heuristiğini
// belgeler: semver prerelease'i düz sürümün altına koyduğu için sıralama
// 1.2.3 / 1.2.3-alpine olur ve kural varyantı seçer. Davranış Faz 0'da
// bilinçli olarak korunuyor.
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
		t.Fatalf("beklenmeyen hata: %v", err)
	}
	if got.Tag != "1.2.3-alpine" {
		t.Errorf("tag = %s, beklenen 1.2.3-alpine", got.Tag)
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
		t.Errorf("hata = %v, beklenen ErrNoMatch", err)
	}
}
