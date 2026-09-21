package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

// httpClient keeps requests from inheriting the default client's lack of a
// timeout. Drivers share it so connections get reused.
var httpClient = &http.Client{Timeout: 20 * time.Second}

// Platform is an os/architecture pair an image is published for.
type Platform struct {
	Arch string
	OS   string
}

// TagInfo carries what a driver knows about a single tag.
type TagInfo struct {
	Tag string

	// Platforms lists the platforms the tag is published for. An empty list
	// means "no platform information", and the platform filter rejects it.
	Platforms []Platform

	// AnyPlatform says the platform filter must not be applied to this tag.
	// Docker Hub records with content_type == "plugin" carry no architecture
	// list and are marked this way.
	AnyPlatform bool

	// LastUpdated is the raw timestamp as the driver reported it. Not every
	// registry can supply one cheaply, so it may be empty: OCI Distribution
	// carries none at all for a multi-architecture index.
	LastUpdated string

	// Digest is the manifest digest the tag points at. It is shown instead of
	// the date when there is none, and it is the right field for an update
	// check.
	Digest string
}

// TagPager reads a repository's tag names one page at a time.
type TagPager interface {
	// Next returns the next page, or (nil, nil) once the pages run out. An
	// empty but non-nil slice means "this page is empty, keep going".
	Next(ctx context.Context) ([]string, error)
}

// Registry is a source of tags for a repository. Both the Docker Hub and the
// OCI Distribution driver implement it.
type Registry interface {
	// Name is the registry name that appears in error messages.
	Name() string

	// Tags returns a reader that pages through the repository's tag names.
	Tags(repo string) TagPager

	// Inspect reports platform and date detail for a single tag. It may hit
	// the network, so resolve only calls it for tags that passed the name
	// filters.
	Inspect(ctx context.Context, repo, tag string) (TagInfo, error)

	// NewestFirst says Tags yields the newest tag on the first page. When it
	// is true, resolve stops paging on the page that produced a match. OCI
	// Distribution returns tags in lexical order, so it reports false.
	NewestFirst() bool
}

// ErrNoMatch is returned when no tag passed the filters.
var ErrNoMatch = errors.New("no matching tag found")

// ErrNoVersion is returned when tags matched the filters but none of them reads
// as a version, so "the latest one" has no answer. Returning whichever the
// registry happened to list first would be a guess dressed up as a result.
var ErrNoVersion = errors.New("no version-like tag found")

const (
	// enoughMatches is how many platform matches resolve needs before it can
	// stop looking: the best tag, plus the runner-up the prefix rule may
	// prefer over it.
	enoughMatches = 2

	// maxInspect bounds how many candidate tags resolve will look up on a
	// registry that cannot order them for us, so that a repository with tens of
	// thousands of tags fails fast with advice instead of issuing one request
	// per tag. gcr.io/distroless/base answers tags/list with ~14 MB of tags,
	// which is what this guards against.
	maxInspect = 50

	// maxBareDigits is the longest a dotted-free numeric tag may be and still
	// count as a version. See isVersionLike.
	maxBareDigits = 4
)

// versionRe matches dot-separated numeric segments, optionally v-prefixed and
// optionally carrying a suffix: 9.8, v2.63.23, 1.37.1-alpine, 24.04.
//
// It is deliberately looser than semver, which rejects a leading zero in a
// segment and would therefore refuse ubuntu:24.04 - one of the most common
// version shapes there is.
var versionRe = regexp.MustCompile(`^v?\d+(\.\d+)*([-+].*)?$`)

// namesOneTag reports whether the filter picks out a single tag by name, in
// which case the user has said which tag they want and the exclusion rules step
// aside: `ldi gcr.io/distroless/base:latest` used to answer "not found" for a
// tag that plainly exists, because latest is on the exclusion list.
//
// A complete literal is the test - "latest", "^latest$", "^1\.2\.3$" - so every
// pattern that actually selects among tags keeps the rules. Note that an empty
// pattern also reports complete, which is the opposite of an explicit request,
// so it is rejected first.
func namesOneTag(filter *regexp.Regexp) bool {
	if filter.String() == "" {
		return false
	}
	_, complete := filter.LiteralPrefix()
	return complete
}

// isVersionLike reports whether a tag reads as a version rather than as a build
// identifier.
//
// semver accepts a bare integer as a major version, so without this check an
// epoch stamp parses as a colossal one: registry.access.redhat.com/ubi9/ubi
// publishes 1789646103 beside 9.8, and v1789646103.0.0 outranks every real
// release. Four digits is the cut-off for a single segment, because node:22,
// python:3 and ubuntu:24 are genuine major versions while 20250101 and
// 1789646103 are a date and an epoch.
//
// Tags that are not numeric at all fail here too, which is what stops a
// repository tagged by commit hash (gcr.io/distroless) from answering with
// whichever hash the registry happened to list first.
func isVersionLike(tag string) bool {
	if !versionRe.MatchString(tag) {
		return false
	}

	// Judge the release part only; a suffix cannot turn a build id into a
	// version.
	core := strings.TrimPrefix(tag, "v")
	if i := strings.IndexAny(core, "-+"); i != -1 {
		core = core[:i]
	}
	if strings.Contains(core, ".") {
		return true
	}
	return len(core) <= maxBareDigits
}

// excludeRe drops tags that are never the answer: pre-release and floating
// tags, source containers, and signature or attestation artifacts. The pattern
// is anchored to separators on purpose: an unbounded `rc` used to silently
// discard valid tags such as "torch", "arch" and "source".
//
// "-source" is excluded as well. Red Hat registries publish a source container
// beside every image (for example "9.0.0-1468-source") and it is not runnable.
// The unbounded `rc` pattern dropped those by accident, since "source" contains
// "rc"; now the rule is stated explicitly.
var excludeRe = regexp.MustCompile(
	`(?i)(^|[-._])(alpha|beta|rc|pre|preview|dev|snapshot|nightly|canary|edge)([-._0-9]|$)` +
		`|^latest([-._]|$)` +
		`|[-._]source$` +
		// cosign writes a sha256-<digest>.sig / .att / .sbom tag beside every
		// image it signs. Those are OCI artifacts, not runnable images, and on
		// a repository like gcr.io/distroless/base they are almost the entire
		// tag list.
		`|\.(sig|att|sbom)$`)

// resolve is the shared selection pipeline: page through the names, filter on
// the name alone, ask for the platform of the survivors only, and return the
// best tag. Querying the platform AFTER the name filter is deliberate, and on a
// driver where that query costs a request it is also asked for as late and as
// rarely as possible.
//
// The two drivers need different strategies, which is what NewestFirst picks
// between. A driver that yields the newest tag first can settle each page as it
// reads it and stop on the page that produced a match - and it only reports
// NewestFirst because its Inspect is free anyway. A driver returning lexical
// order knows nothing about which tag is newest, so the candidates are all
// collected, sorted, and only then looked up from the top down. Inspecting as
// they arrive instead would cost one request per tag: gcr.io/distroless/base
// has tens of thousands.
func resolve(ctx context.Context, reg Registry, repo string, filter *regexp.Regexp, arch, osName string) (TagInfo, error) {
	pager := reg.Tags(repo)

	// A filter naming one tag outright is a request for that tag, not a query
	// to be second-guessed.
	explicit := namesOneTag(filter)

	var (
		matches    []TagInfo
		candidates []string

		// Remembered for the failure messages: what the tags of this repository
		// look like, whether any was a version at all, and what the exclusion
		// rules threw away.
		notAVersion string
		sawAVersion bool
		excluded    int
		excludedEg  string
	)

	for {
		names, err := pager.Next(ctx)
		if err != nil {
			return TagInfo{}, err
		}
		if names == nil {
			break
		}

		page := make([]string, 0, len(names))
		for _, name := range names {
			if !filter.MatchString(name) {
				continue
			}

			if !explicit {
				if excludeRe.MatchString(name) {
					excluded++
					if excludedEg == "" {
						excludedEg = name
					}
					continue
				}
				if !isVersionLike(name) {
					if notAVersion == "" {
						notAVersion = name
					}
					continue
				}
				sawAVersion = true
			}

			page = append(page, name)
		}

		if !reg.NewestFirst() {
			candidates = append(candidates, page...)
			continue
		}

		for _, name := range page {
			info, err := reg.Inspect(ctx, repo, name)
			if err != nil {
				return TagInfo{}, err
			}
			if platformMatches(info, arch, osName) {
				matches = append(matches, info)
			}
		}
		if len(matches) > 0 {
			break
		}
	}

	if !reg.NewestFirst() {
		sortTagNames(candidates)

		for i, name := range candidates {
			if len(matches) >= enoughMatches {
				break
			}
			if i >= maxInspect {
				return TagInfo{}, fmt.Errorf(
					"%s: looked up %d of %d candidate tags without finding one for %s/%s; narrow the tag filter",
					reg.Name(), maxInspect, len(candidates), osName, arch)
			}

			info, err := reg.Inspect(ctx, repo, name)
			if err != nil {
				return TagInfo{}, err
			}
			if platformMatches(info, arch, osName) {
				matches = append(matches, info)
			}
		}
	}

	if len(matches) == 0 {
		switch {
		case !sawAVersion && notAVersion != "":
			return TagInfo{}, fmt.Errorf(
				"%w: the tags of %s look like %q; add a tag filter to pick one",
				ErrNoVersion, repo, notAVersion)

		case excluded > 0:
			// Reported rather than swallowed as "not found": the tags are
			// there, they were just ruled out.
			return TagInfo{}, fmt.Errorf(
				"%d tag(s) of %s matched but are excluded as pre-release or floating, such as %q; name the tag exactly to select it",
				excluded, repo, excludedEg)
		}
		return TagInfo{}, ErrNoMatch
	}

	sortTags(matches)

	best := matches[0]
	if len(matches) > 1 {
		next := matches[1]
		// Prefer the more specific one for cases like "1.0" and "1.0.1".
		// NOTE: combined with semver ordering this rule can pick
		// "1.2.3-alpine" over "1.2.3". The behaviour is kept deliberately;
		// the README recommends anchoring the regex with `$`.
		if strings.HasPrefix(next.Tag, best.Tag) && len(next.Tag) > len(best.Tag) {
			best = next
		}
	}
	return best, nil
}

// platformMatches reports whether the tag is published for the requested
// os/architecture.
func platformMatches(info TagInfo, arch, osName string) bool {
	if info.AnyPlatform {
		return true
	}
	for _, p := range info.Platforms {
		if p.Arch == arch && p.OS == osName {
			return true
		}
	}
	return false
}

// sortTags orders tags newest first, highest version at the front.
//
// Equal versions are settled by date where the driver supplied one, which in
// practice means Docker Hub: its API returns last_updated in the same call, so
// the tie-break is free there. OCI Distribution reports no date for a
// multi-architecture index, and those entries simply keep their relative order.
// RFC 3339 timestamps compare correctly as plain strings.
func sortTags(tags []TagInfo) {
	slices.SortFunc(tags, func(a, b TagInfo) int {
		if c := compareTags(a.Tag, b.Tag); c != 0 {
			return c
		}
		return -strings.Compare(a.LastUpdated, b.LastUpdated)
	})
}

// sortTagNames is sortTags over bare names, for ordering candidates before any
// of them has been looked up.
func sortTagNames(names []string) {
	slices.SortFunc(names, compareTags)
}

// compareTags returns a negative value when a should come before b. A
// -security- rebuild of the same base version wins over the plain release.
func compareTags(a, b string) int {
	v1 := a
	if !strings.HasPrefix(v1, "v") {
		v1 = "v" + v1
	}
	v2 := b
	if !strings.HasPrefix(v2, "v") {
		v2 = "v" + v2
	}

	isSec1 := strings.Contains(v1, "security")
	isSec2 := strings.Contains(v2, "security")

	if isSec1 != isSec2 {
		base1 := getBaseVersion(v1)
		base2 := getBaseVersion(v2)
		if base1 == base2 {
			if isSec1 {
				return -1
			}
			return 1
		}
	}
	return -semver.Compare(v1, v2)
}

// getBaseVersion returns the canonical base version without any pre-release
// or build metadata. Example: "v13.0.1-security-01" -> "v13.0.1".
func getBaseVersion(v string) string {
	c := semver.Canonical(v)
	if c == "" {
		c = v
	}
	if idx := strings.IndexAny(c, "-+"); idx != -1 {
		return c[:idx]
	}
	return c
}

// Reference material for further registry work:
// https://quay.io/api/v1/repository/prometheus/node-exporter/tag/?limit=100&page=1&onlyActiveTags=true
// https://quay.io/api/v1/repository/prometheus/node-exporter/manifest/sha256:...
// https://github.com/shogo82148/docker-image-update-checker/blob/main/registry/registry.go
