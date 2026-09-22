package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
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

// lookupStats records what a lookup cost. Reported by -verbose, because it is
// the only way to tell a slow registry from a slow strategy: gcr.io answers
// tags/list with one response of about 14 MB, public.ecr.aws paginates a much
// longer list, and until these numbers existed the difference was guesswork.
type lookupStats struct {
	// Pages of tag names read from the registry.
	Pages int
	// Tags seen across those pages, before any filtering.
	Tags int
	// Candidates left after the name filters.
	Candidates int
	// Lookups is the number of Inspect calls, each of which costs a request on
	// a Distribution registry and nothing on Docker Hub.
	Lookups int
}

// ErrNoVersion is returned when tags matched the filters but none of them reads
// as a version, so "the latest one" has no answer. Returning whichever the
// registry happened to list first would be a guess dressed up as a result.
var ErrNoVersion = errors.New("no version-like tag found")

const (
	// maxInspect bounds how many candidate tags resolve will look up on a
	// registry that cannot order them for us, so that a repository with tens of
	// thousands of tags fails fast with advice instead of issuing one request
	// per tag. gcr.io/distroless/base answers tags/list with ~14 MB of tags,
	// which is what this guards against.
	maxInspect = 50

	// maxBareDigits is the longest a dotted-free numeric tag may be and still
	// count as a version. See isVersionLike.
	maxBareDigits = 4

	// maxOrderedPages bounds the extra pages read when a registry's date
	// order disagrees with version order. Every page has to improve on the
	// best version so far to earn the next one, so this is only reached by a
	// repository whose versions keep climbing as the dates fall - which does
	// not last long. It is insurance, not a working limit.
	maxOrderedPages = 10
)

// versionRe matches dot-separated numeric segments, optionally v-prefixed and
// optionally carrying a suffix: 9.8, v2.63.23, 1.37.1-alpine, 24.04.
//
// It is deliberately looser than semver, which rejects a leading zero in a
// segment and would therefore refuse ubuntu:24.04 - one of the most common
// version shapes there is.
var versionRe = regexp.MustCompile(`^v?\d+(\.\d+)*([-+].*)?$`)

// namedTag returns the one tag a filter asks for by name, if it does. The user
// has then said which tag they want, so the exclusion rules and the
// version-like requirement step aside: `ldi gcr.io/distroless/base:latest` used
// to answer "not found" for a tag that plainly exists, because latest is on the
// exclusion list.
//
// A complete literal is the test - "latest", "^latest$", "^1\.2\.3$" - so every
// pattern that actually selects among tags keeps the rules. Two traps: the empty
// pattern also reports complete, which is the opposite of an explicit request,
// and a bare literal is an UNANCHORED regex, so matching with it would accept
// any tag containing the word. `ldi grafana/loki:latest` answered latest-amd64
// that way. The returned name is therefore compared for equality, not matched.
func namedTag(filter *regexp.Regexp) (string, bool) {
	if filter.String() == "" {
		return "", false
	}
	name, complete := filter.LiteralPrefix()
	if !complete {
		return "", false
	}
	return name, true
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
func resolve(ctx context.Context, reg Registry, repo string, filter *regexp.Regexp, arch, osName string) (TagInfo, lookupStats, error) {
	var st lookupStats

	pager := reg.Tags(repo)

	// A filter naming one tag outright is a request for that tag, not a query
	// to be second-guessed.
	wanted, explicit := namedTag(filter)

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
			return TagInfo{}, st, err
		}
		if names == nil {
			break
		}
		st.Pages++
		st.Tags += len(names)

		page := make([]string, 0, len(names))
		for _, name := range names {
			if explicit {
				if name == wanted {
					page = append(page, name)
				}
				continue
			}

			if !filter.MatchString(name) {
				continue
			}
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
			page = append(page, name)
		}
		st.Candidates += len(page)

		if !reg.NewestFirst() {
			candidates = append(candidates, page...)
			// An exact name matches at most one tag, so once it is in hand
			// there is nothing in the rest of the list to find.
			if explicit && len(candidates) > 0 {
				break
			}
			continue
		}

		var found []TagInfo
		for _, name := range page {
			st.Lookups++
			info, err := reg.Inspect(ctx, repo, name)
			if err != nil {
				return TagInfo{}, st, err
			}
			if platformMatches(info, arch, osName) {
				found = append(found, info)
			}
		}
		if len(found) == 0 {
			continue
		}

		previous := bestTag(matches)
		matches = append(matches, found...)
		best := bestTag(matches)

		// An exact name matches at most one tag, so the first page that
		// carries it has the answer.
		if explicit {
			break
		}

		// These pages are ordered by date and the question is about
		// versions, so the page that first matched is not evidence that
		// nothing better lies behind it. grafana/grafana-oss is the case
		// that showed it: of the hundred most recently updated tags the
		// winning 13.0.2 was three months old, and ninety-odd newer
		// entries were patches to older release lines. One more page of
		// that and the answer would have fallen out of the window.
		//
		// Reading one page further is the price; it is a request on Docker
		// Hub and on the Red Hat catalogue, and nothing on an unordered
		// registry, which collects everything anyway. A page has to
		// improve on the best version seen to earn the next one.
		//
		// This is a bound on the damage, not a guarantee: a release line
		// that has been quiet for several pages can still be missed.
		// Pinning the major in the filter is what makes the answer certain.
		if previous != "" && best == previous {
			break
		}
		if st.Pages >= maxOrderedPages {
			break
		}
	}

	if !reg.NewestFirst() {
		sortTagNames(candidates)

		for i, name := range candidates {
			// The list is sorted, so the first tag published for the requested
			// platform is the answer; there is nothing a later one could win.
			if len(matches) > 0 {
				break
			}
			if i >= maxInspect {
				return TagInfo{}, st, fmt.Errorf(
					"%s: looked up %d of %d candidate tags without finding one for %s/%s; narrow the tag filter",
					reg.Name(), maxInspect, len(candidates), osName, arch)
			}

			st.Lookups++
			info, err := reg.Inspect(ctx, repo, name)
			if err != nil {
				return TagInfo{}, st, err
			}
			if platformMatches(info, arch, osName) {
				matches = append(matches, info)
			}
		}
	}

	if len(matches) == 0 {
		switch {
		case !sawAVersion && notAVersion != "":
			return TagInfo{}, st, fmt.Errorf(
				"%w: the tags of %s look like %q; add a tag filter to pick one",
				ErrNoVersion, repo, notAVersion)

		case excluded > 0:
			// Reported rather than swallowed as "not found": the tags are
			// there, they were just ruled out.
			return TagInfo{}, st, fmt.Errorf(
				"%d tag(s) of %s matched but are excluded as pre-release or floating, such as %q; name the tag exactly to select it",
				excluded, repo, excludedEg)
		}
		return TagInfo{}, st, ErrNoMatch
	}

	sortTags(matches)
	return matches[0], st, nil
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

// bestTag is the tag that would win among these matches, without disturbing
// the order they were found in - that order is the registry's, and the caller
// still needs it to tell newest-by-date from highest-by-version.
func bestTag(matches []TagInfo) string {
	if len(matches) == 0 {
		return ""
	}
	sorted := slices.Clone(matches)
	sortTags(sorted)
	return sorted[0].Tag
}

// sortTagNames is sortTags over bare names, for ordering candidates before any
// of them has been looked up.
func sortTagNames(names []string) {
	slices.SortFunc(names, compareTags)
}

// compareTags returns a negative value when a should come before b.
//
// The numeric core is compared segment by segment as integers rather than
// through semver, because semver cannot parse much of what registries publish:
// more than three segments (public.ecr.aws/lambda/nodejs tags look like
// 22.2025.04.24.11) or a leading zero inside one (ubuntu 24.04). It returned 0
// for every such pair, which left those tags in whatever order the registry
// happened to list them - and on Docker Hub the answer only came out right
// because sortTags falls back to the date. For a core semver can parse the two
// agree, so nothing that already worked moves.
func compareTags(a, b string) int {
	coreA, suffixA := splitTag(a)
	coreB, suffixB := splitTag(b)

	if c := compareSegments(coreA, coreB); c != 0 {
		return -c
	}

	// Same version: a -security- rebuild supersedes the plain release.
	secA := strings.Contains(a, "security")
	secB := strings.Contains(b, "security")
	if secA != secB {
		if secA {
			return -1
		}
		return 1
	}

	// Otherwise the plain release outranks a suffixed build of it, which is what
	// keeps 3.7.8 ahead of 3.7.8-amd64.
	switch {
	case suffixA == "" && suffixB != "":
		return -1
	case suffixA != "" && suffixB == "":
		return 1
	case suffixA == suffixB:
		return 0
	}

	// Both carry a suffix. semver orders real pre-releases properly; where it
	// cannot parse them, the later suffix comes first.
	if c := semver.Compare("v"+a, "v"+b); c != 0 {
		return -c
	}
	return strings.Compare(suffixB, suffixA)
}

// splitTag separates a tag into its numeric segments and whatever follows them:
// "v1.37.1-alpine" becomes [1 37 1] and "-alpine". A non-numeric segment yields
// no core at all, so tags that are not version-shaped compare as equal and keep
// their order.
func splitTag(tag string) ([]int, string) {
	s := strings.TrimPrefix(tag, "v")

	core, suffix := s, ""
	if i := strings.IndexAny(s, "-+"); i != -1 {
		core, suffix = s[:i], s[i:]
	}

	var segments []int
	for _, part := range strings.Split(core, ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, suffix
		}
		segments = append(segments, n)
	}
	return segments, suffix
}

// compareSegments compares two segment lists numerically, treating a missing
// segment as zero so that 1.2 and 1.2.0 are the same version. Numeric beats
// lexical here: 1.10.0 is above 1.9.0.
func compareSegments(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		x, y := 0, 0
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// Reference material for further registry work:
// https://quay.io/api/v1/repository/prometheus/node-exporter/tag/?limit=100&page=1&onlyActiveTags=true
// https://quay.io/api/v1/repository/prometheus/node-exporter/manifest/sha256:...
// https://github.com/shogo82148/docker-image-update-checker/blob/main/registry/registry.go
