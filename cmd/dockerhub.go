package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp/syntax"
	"strings"
)

// hubTag is a single record in a hub.docker.com/v2/.../tags response.
type hubTag struct {
	Name        string     `json:"name"`
	ContentType string     `json:"content_type"`
	LastUpdated string     `json:"last_updated"`
	Images      []hubImage `json:"images"`
}

type hubImage struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Status       string `json:"status"`
}

type hubResponse struct {
	Next    string   `json:"next"`
	Results []hubTag `json:"results"`
}

// hubBaseURL is the root of the Docker Hub API. Tests point dockerHub.baseURL
// at their own server instead.
const hubBaseURL = "https://hub.docker.com"

// dockerHub uses Docker Hub's own (proprietary) hub.docker.com/v2 API. That
// API answers with tag, platform and date in one call, so Inspect never hits
// the network: it reads the records collected while paging.
type dockerHub struct {
	// nameFilter goes to the API's server-side `name=` substring filter.
	nameFilter string
	// seen holds the raw records observed while paging (tag name -> record).
	seen map[string]hubTag
	// baseURL is the API root; only tests change it.
	baseURL string
}

func newDockerHub(nameFilter string) *dockerHub {
	return &dockerHub{
		nameFilter: nameFilter,
		seen:       make(map[string]hubTag),
		baseURL:    hubBaseURL,
	}
}

func (h *dockerHub) Name() string { return "docker hub" }

// NewestFirst: the query uses ordering=last_updated, so the first page carries
// the newest tags.
func (h *dockerHub) NewestFirst() bool { return true }

// Tags ignores the prefix: this API has its own server-side substring filter,
// which requiredLiteral already feeds and which accepts a literal from anywhere
// in the pattern rather than only the start.
func (h *dockerHub) Tags(repo, _ string) TagPager {
	// On Docker Hub a single-component name lives in the "library" namespace.
	if !strings.Contains(repo, "/") {
		repo = "library/" + repo
	}

	q := url.Values{
		"page_size": {"100"},
		"status":    {"active"},
		"page":      {"1"},
		"ordering":  {"last_updated"},
	}
	if h.nameFilter != "" {
		q.Set("name", h.nameFilter)
	}

	return &hubPager{
		hub: h,
		url: fmt.Sprintf("%s/v2/repositories/%s/tags?%s", h.baseURL, repo, q.Encode()),
	}
}

func (h *dockerHub) Inspect(_ context.Context, _, tag string) (TagInfo, error) {
	t, ok := h.seen[tag]
	if !ok {
		return TagInfo{}, fmt.Errorf("%s: tag was not seen while paging", tag)
	}

	info := TagInfo{Tag: t.Name, LastUpdated: t.LastUpdated}
	switch t.ContentType {
	case "plugin":
		// Plugin records carry no architecture list.
		info.AnyPlatform = true
	case "image":
		for _, img := range t.Images {
			if img.Status != "active" {
				continue
			}
			info.Platforms = append(info.Platforms, Platform{Arch: img.Architecture, OS: img.OS})
		}
	}
	return info, nil
}

// hubPager walks the pages by following the "next" field in the response.
type hubPager struct {
	hub  *dockerHub
	url  string
	done bool
}

func (p *hubPager) Next(ctx context.Context) ([]string, error) {
	if p.done {
		return nil, nil
	}

	resp, err := p.hub.fetch(ctx, p.url)
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(resp.Results))
	for _, t := range resp.Results {
		p.hub.seen[t.Name] = t
		names = append(names, t.Name)
	}

	if resp.Next == "" {
		p.done = true
	} else {
		p.url = resp.Next
	}
	return names, nil
}

func (h *dockerHub) fetch(ctx context.Context, repoURL string) (*hubResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, repoURL, nil)
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

	// Without a status check a 404 or 429 body ({"message": ...}) decodes
	// cleanly, Results stays empty, and the real reason is hidden behind
	// "Not found".
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		if msg := apiMessage(body); msg != "" {
			return nil, fmt.Errorf("%s HTTP %d: %s", h.Name(), resp.StatusCode, msg)
		}
		return nil, fmt.Errorf("%s HTTP %d: unexpected response", h.Name(), resp.StatusCode)
	}

	var r hubResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("%s: could not read the tag list: %w", h.Name(), err)
	}
	return &r, nil
}

// apiMessage extracts the description from a Docker Hub error body.
func apiMessage(body []byte) string {
	var e struct {
		Message string `json:"message"`
		Detail  string `json:"detail"`
	}
	if json.Unmarshal(body, &e) != nil {
		return ""
	}
	if e.Message != "" {
		return e.Message
	}
	return e.Detail
}

// requiredLiteral returns a literal part that EVERY match of re must contain,
// or "" when there is none. The result goes to Docker Hub's `name=` substring
// filter, where a wrong literal would silently hide valid tags, so only nodes
// that take part in every match are descended into. Alternations and optional
// groups are skipped: "alpine|bookworm" yields "" rather than picking "alpine"
// and losing every bookworm tag. Literals shorter than 2 characters are
// ignored, since they make no useful filter.
func requiredLiteral(re *syntax.Regexp) string {
	switch re.Op {
	case syntax.OpLiteral:
		// A case-folded literal may not agree with the server-side filter.
		if len(re.Rune) < 2 || re.Flags&syntax.FoldCase != 0 {
			return ""
		}
		return string(re.Rune)

	case syntax.OpConcat, syntax.OpCapture, syntax.OpPlus:
		// These nodes appear at least once in every match.
		for _, sub := range re.Sub {
			if lit := requiredLiteral(sub); lit != "" {
				return lit
			}
		}

	case syntax.OpRepeat:
		if re.Min >= 1 {
			for _, sub := range re.Sub {
				if lit := requiredLiteral(sub); lit != "" {
					return lit
				}
			}
		}
	}
	return ""
}
