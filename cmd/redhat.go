package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// Red Hat's container catalogue - the Pyxis API behind catalog.redhat.com -
// answers with architecture, build date, digest and the tag names of a build
// in a single request, and it can sort by build date. Over OCI Distribution
// the same repository costs 34 sequential requests and about ten seconds,
// because registry.access.redhat.com caps a page at 100 tag names and carries
// no dates at all.
//
// Measured on ubi9/ubi: 10.08s over Distribution against about 1.3s here, and
// the ordering rests on real dates rather than on reading the tag names.
//
// Only registry.access.redhat.com is indexed. The catalogue answers
// registry.redhat.io with an empty list, so that host keeps the Distribution
// driver - and still needs credentials.
const (
	redHatHost = "registry.access.redhat.com"

	// redHatAPI is the catalogue root. Tests point redHat.baseURL elsewhere.
	redHatAPI = "https://catalog.redhat.com/api/containers/v1"

	// redHatPageSize is how many image records to ask for at once. The API
	// refuses more than 500; 100 is the point where one page still arrives in
	// about a second and already covers the newest hundred builds, which is
	// far more than a lookup normally needs.
	redHatPageSize = 100

	// redHatMaxPages bounds the walk. At 100 records a page this is more
	// history than any repository here has: ubi9/ubi holds 600 records in
	// total, across every architecture.
	redHatMaxPages = 20
)

// rhTag is one tag of one build.
type rhTag struct {
	Name      string `json:"name"`
	AddedDate string `json:"added_date"`
}

// rhRepo is where a build is published. One image record can be published to
// several repositories (ubi9/ubi and ubi9 both appear), so the entry has to be
// matched on registry and repository before its tags are believed.
type rhRepo struct {
	Registry   string `json:"registry"`
	Repository string `json:"repository"`
	Published  bool   `json:"published"`

	// ManifestListDigest is what the tag resolves to for a multi-architecture
	// build; ManifestDigest is the single-architecture manifest.
	ManifestListDigest string `json:"manifest_list_digest"`
	ManifestDigest     string `json:"manifest_schema2_digest"`

	Tags []rhTag `json:"tags"`
}

// rhImage is one build for one architecture. The same tag therefore appears
// once per architecture, and Inspect merges those into a single TagInfo.
type rhImage struct {
	Architecture string   `json:"architecture"`
	CreationDate string   `json:"creation_date"`
	Repositories []rhRepo `json:"repositories"`
}

type rhResponse struct {
	Data     []rhImage `json:"data"`
	Total    int       `json:"total"`
	Page     int       `json:"page"`
	PageSize int       `json:"page_size"`
}

// redHat reads tags from the Red Hat container catalogue. Like the Docker Hub
// driver it collects everything while paging, so Inspect never hits the
// network.
type redHat struct {
	// arch goes to the catalogue's server-side architecture filter. With it
	// set, a page of 100 records is 100 builds of the requested architecture
	// rather than 25 builds times four architectures.
	arch string

	// seen holds what paging has established about each tag.
	seen map[string]TagInfo

	// baseURL is the API root; only tests change it.
	baseURL string
}

func newRedHat(arch string) *redHat {
	return &redHat{
		arch:    arch,
		seen:    make(map[string]TagInfo),
		baseURL: redHatAPI,
	}
}

func (r *redHat) Name() string { return redHatHost }

// NewestFirst: the query sorts by creation_date descending, so the first page
// carries the newest builds.
func (r *redHat) NewestFirst() bool { return true }

func (r *redHat) Tags(repo string) TagPager {
	return &rhPager{rh: r, repo: repo, emitted: make(map[string]bool)}
}

func (r *redHat) Inspect(_ context.Context, _, tag string) (TagInfo, error) {
	info, ok := r.seen[tag]
	if !ok {
		return TagInfo{}, fmt.Errorf("%s: tag was not seen while paging", tag)
	}
	return info, nil
}

// record merges one build's view of a tag into what is already known. A tag
// published for four architectures arrives as four records, and the platform
// list has to end up carrying all of them.
func (r *redHat) record(repo rhRepo, img rhImage, tag rhTag) {
	info := r.seen[tag.Name]
	info.Tag = tag.Name

	// The catalogue indexes Linux images only; it reports no operating system
	// field, and registry.access.redhat.com publishes nothing else.
	p := Platform{Arch: img.Architecture, OS: "linux"}
	if !containsPlatform(info.Platforms, p) {
		info.Platforms = append(info.Platforms, p)
	}

	// added_date is when this tag was attached to this build, which is the
	// date a lookup wants. creation_date is the build itself.
	when := tag.AddedDate
	if when == "" {
		when = img.CreationDate
	}
	if when > info.LastUpdated {
		info.LastUpdated = when
	}

	if info.Digest == "" {
		info.Digest = repo.ManifestListDigest
		if info.Digest == "" {
			info.Digest = repo.ManifestDigest
		}
	}

	r.seen[tag.Name] = info
}

func containsPlatform(list []Platform, p Platform) bool {
	for _, q := range list {
		if q == p {
			return true
		}
	}
	return false
}

// rhPager walks the catalogue a page of image records at a time.
type rhPager struct {
	rh   *redHat
	repo string
	page int
	done bool

	// emitted keeps a tag from being reported twice. This is only safe while
	// the architecture filter is on, because then every record on every page
	// is the same architecture and a repeat says nothing new. Without the
	// filter a tag could reappear on a later page carrying the architecture
	// that was wanted, so there the name is emitted again.
	emitted map[string]bool
}

func (p *rhPager) Next(ctx context.Context) ([]string, error) {
	if p.done {
		return nil, nil
	}

	resp, err := p.rh.fetch(ctx, p.url())
	if err != nil {
		return nil, err
	}

	var names []string
	onPage := make(map[string]bool)

	for _, img := range resp.Data {
		for _, repo := range img.Repositories {
			if repo.Registry != redHatHost || repo.Repository != p.repo {
				continue
			}
			for _, tag := range repo.Tags {
				p.rh.record(repo, img, tag)

				if onPage[tag.Name] {
					continue
				}
				onPage[tag.Name] = true

				if p.rh.arch != "" && p.emitted[tag.Name] {
					continue
				}
				p.emitted[tag.Name] = true
				names = append(names, tag.Name)
			}
		}
	}

	p.page++
	switch {
	case len(resp.Data) < redHatPageSize:
		p.done = true
	case resp.Total > 0 && p.page*redHatPageSize >= resp.Total:
		p.done = true
	case p.page >= redHatMaxPages:
		p.done = true
	}

	// A page whose records all belonged to another repository is empty but not
	// final: returning a non-nil slice keeps resolve paging.
	if names == nil {
		names = []string{}
	}
	return names, nil
}

func (p *rhPager) url() string {
	q := url.Values{
		"page":      {strconv.Itoa(p.page)},
		"page_size": {strconv.Itoa(redHatPageSize)},
		// The projection is what makes this fast. Asking for the whole record
		// costs about 6 KB per build and, oddly, minutes rather than seconds
		// on a sorted query. Leaf fields inside tags cannot be named
		// individually - the API rejects them - so data.repositories is taken
		// whole.
		"include": {"data.architecture,data.creation_date,data.repositories"},
		"sort_by": {"creation_date[desc]"},
	}
	if p.rh.arch != "" {
		q.Set("filter", "architecture=="+p.rh.arch)
	}

	return fmt.Sprintf("%s/repositories/registry/%s/repository/%s/images?%s",
		p.rh.baseURL, redHatHost, p.repo, q.Encode())
}

func (r *redHat) fetch(ctx context.Context, apiURL string) (*rhResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ldi/"+Version)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		if msg := rhMessage(body); msg != "" {
			return nil, fmt.Errorf("%s catalog HTTP %d: %s", r.Name(), resp.StatusCode, msg)
		}
		return nil, fmt.Errorf("%s catalog HTTP %d: unexpected response", r.Name(), resp.StatusCode)
	}

	var out rhResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("%s: could not read the catalog response: %w", r.Name(), err)
	}
	return &out, nil
}

// rhMessage pulls the description out of a catalogue error body, which follows
// RFC 7807: {"type":..., "title":..., "detail":..., "status":...}.
func rhMessage(body []byte) string {
	var e struct {
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	if json.Unmarshal(body, &e) != nil {
		return ""
	}
	if e.Detail != "" {
		return e.Detail
	}
	return e.Title
}
