package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// The tag list has to be read in full, because lexical order says nothing about
// which tag is newest, so the page count is the whole cost of a lookup:
// public.ecr.aws/lambda/nodejs has 8825 tags, and at 100 per page that meant 89
// sequential requests and 43 seconds, with the tag filter making no difference
// whatsoever. A large page is therefore asked for. The spec lets a registry
// return fewer than requested, so this is a request and not an assumption; one
// that rejects the size outright gets a single retry at the modest value.
const (
	tagPageSize     = 1000
	tagPageSizeSafe = 100
)

// OCI Distribution manifest media types. All of them are asked for together in
// the Accept header: the server answers with a multi-architecture index when it
// has one, and a single manifest otherwise.
const (
	mediaOCIIndex       = "application/vnd.oci.image.index.v1+json"
	mediaOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	mediaDockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
)

var manifestAccept = strings.Join([]string{
	mediaOCIIndex, mediaDockerList, mediaOCIManifest, mediaDockerManifest,
}, ", ")

// distribution is the driver for registries speaking OCI Distribution (formerly
// the Docker Registry HTTP API v2): registry.redhat.io,
// registry.access.redhat.com, quay.io, ghcr.io, Harbor and so on.
//
// This API gives far less than Docker Hub's proprietary one: tags/list returns
// names only, and the platform costs one manifest request per tag. That is why
// resolve calls Inspect solely for the candidates that passed the name filter.
//
// Current scope: anonymous access. The token handshake is performed but no
// credentials are sent; reading auth.json / config.json comes next.
type distribution struct {
	host string
	// baseURL is the registry root; only tests change it.
	baseURL string
	// tokens caches the Bearer token per repository. Tokens are repository
	// scoped and short lived, so keeping them for the life of the process is
	// enough.
	tokens map[string]string
}

func newDistribution(host string) *distribution {
	return &distribution{
		host:    host,
		baseURL: "https://" + host,
		tokens:  make(map[string]string),
	}
}

func (d *distribution) Name() string { return d.host }

// NewestFirst: the OCI specification requires tags in lexical ("ASCIIbetical")
// order, so the first page does not carry the newest version. Paging therefore
// cannot stop early; the whole list has to be scanned.
func (d *distribution) NewestFirst() bool { return false }

// Tags starts the listing at prefix when one is given. Tags arrive in lexical
// order, and "last=22." is strictly before "22.0", so the whole block that can
// match is still returned while everything below it is skipped. That is the
// only lever left on this driver: the cost of a lookup tracks the number of
// tags read, not the number of requests - public.ecr.aws answered 8825 tags in
// 43s over 89 pages and 28.8s over 9, about 3ms a tag either way.
func (d *distribution) Tags(repo, prefix string) TagPager {
	return &distPager{
		dist:   d,
		repo:   repo,
		prefix: prefix,
		url:    d.tagsURL(repo, tagPageSize, prefix),
		first:  true,
	}
}

func (d *distribution) tagsURL(repo string, pageSize int, prefix string) string {
	q := url.Values{"n": {strconv.Itoa(pageSize)}}
	if prefix != "" {
		q.Set("last", prefix)
	}
	return fmt.Sprintf("%s/v2/%s/tags/list?%s", d.baseURL, repo, q.Encode())
}

// distPager walks the pages by following the rel="next" link in the Link
// header.
type distPager struct {
	dist   *distribution
	repo   string
	prefix string
	url    string
	done   bool
	// first marks the request built here rather than taken from a Link header,
	// which is the only one whose page size is ours to retry.
	first bool
}

func (p *distPager) Next(ctx context.Context) ([]string, error) {
	if p.done {
		return nil, nil
	}

	resp, err := p.dist.get(ctx, p.repo, p.url, "application/json")
	if err != nil && p.first && isBadRequest(err) {
		// The registry refused the page size. Ask for the modest one rather
		// than failing the whole lookup over an optimisation.
		p.url = p.dist.tagsURL(p.repo, tagPageSizeSafe, p.prefix)
		resp, err = p.dist.get(ctx, p.repo, p.url, "application/json")
	}
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	p.first = false

	var body struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	// The cap is generous rather than tight: gcr.io/distroless/base legitimately
	// answers with about 14 MB, since it does not paginate at all.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 128<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("%s: could not read the tag list: %w", p.dist.host, err)
	}

	if next := nextLink(resp.Header.Get("Link"), p.url); next != "" {
		p.url = next
	} else {
		p.done = true
	}

	// An empty repository can answer with "tags": null. Returning nil would
	// mean "no more pages", so it is turned into an empty slice.
	if body.Tags == nil {
		return []string{}, nil
	}
	return body.Tags, nil
}

func (d *distribution) Inspect(ctx context.Context, repo, tag string) (TagInfo, error) {
	resp, err := d.get(ctx, repo, fmt.Sprintf("%s/v2/%s/manifests/%s", d.baseURL, repo, tag), manifestAccept)
	if err != nil {
		return TagInfo{}, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return TagInfo{}, fmt.Errorf("%s: could not read the manifest of %s: %w", d.host, tag, err)
	}

	info := TagInfo{Tag: tag, Digest: resp.Header.Get("Docker-Content-Digest")}

	var m struct {
		Manifests []struct {
			Platform struct {
				Architecture string `json:"architecture"`
				OS           string `json:"os"`
			} `json:"platform"`
		} `json:"manifests"`
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return TagInfo{}, fmt.Errorf("%s: could not parse the manifest of %s: %w", d.host, tag, err)
	}

	// Multi-architecture index: the platform list is in the manifest itself.
	if len(m.Manifests) > 0 {
		for _, sub := range m.Manifests {
			// buildkit's attestation and signature records arrive with an
			// "unknown/unknown" platform; they are not runnable images.
			if sub.Platform.Architecture == "" || sub.Platform.Architecture == "unknown" {
				continue
			}
			info.Platforms = append(info.Platforms, Platform{
				Arch: sub.Platform.Architecture,
				OS:   sub.Platform.OS,
			})
		}
		return info, nil
	}

	// Single manifest: the platform is not in the manifest but in the config
	// blob, so only single-architecture images cost one extra request.
	if m.Config.Digest == "" {
		return info, nil
	}

	cfg, err := d.imageConfig(ctx, repo, m.Config.Digest)
	if err != nil {
		return TagInfo{}, err
	}
	if cfg.Architecture != "" {
		info.Platforms = append(info.Platforms, Platform{Arch: cfg.Architecture, OS: cfg.OS})
	}
	// config.created is the image build time: the closest counterpart to
	// Docker Hub's last_updated, and free only along this path.
	info.LastUpdated = cfg.Created

	return info, nil
}

type imageConfig struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Created      string `json:"created"`
}

func (d *distribution) imageConfig(ctx context.Context, repo, digest string) (imageConfig, error) {
	resp, err := d.get(ctx, repo, fmt.Sprintf("%s/v2/%s/blobs/%s", d.baseURL, repo, digest), "application/json")
	if err != nil {
		return imageConfig{}, err
	}
	defer resp.Body.Close()

	var cfg imageConfig
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&cfg); err != nil {
		return imageConfig{}, fmt.Errorf("%s: could not read the config blob: %w", d.host, err)
	}
	return cfg, nil
}

// get performs the request and, on a 401, takes a token from the challenge and
// retries once. The caller closes the body of the returned response.
func (d *distribution) get(ctx context.Context, repo, rawURL, accept string) (*http.Response, error) {
	resp, err := d.do(ctx, rawURL, accept, d.tokens[repo])
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("Www-Authenticate")
		resp.Body.Close()

		token, err := d.fetchToken(ctx, repo, challenge)
		if err != nil {
			return nil, err
		}
		d.tokens[repo] = token

		if resp, err = d.do(ctx, rawURL, accept, token); err != nil {
			return nil, err
		}
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, d.statusError(resp.StatusCode, body)
	}
	return resp, nil
}

func (d *distribution) do(ctx context.Context, rawURL, accept, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "ldi/"+Version)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return httpClient.Do(req)
}

// fetchToken reads the realm and service from the WWW-Authenticate challenge of
// a 401 response and asks for a token. The realm is never hardcoded; every
// registry announces its own address in that header.
//
// Next step: credentials (a Registry Service Account) will be added here with
// req.SetBasicAuth. As it stands it takes an anonymous token, which is enough
// for public content on registry.access.redhat.com, quay.io and ghcr.io.
func (d *distribution) fetchToken(ctx context.Context, repo, challenge string) (string, error) {
	scheme, params := parseChallenge(challenge)
	if scheme == "" {
		return "", fmt.Errorf("%s: the 401 response carried no WWW-Authenticate header", d.host)
	}
	if !strings.EqualFold(scheme, "bearer") {
		return "", fmt.Errorf("%s: unsupported authentication scheme %q", d.host, scheme)
	}

	realm := params["realm"]
	if realm == "" {
		return "", fmt.Errorf("%s: the authentication challenge carried no realm", d.host)
	}

	u, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("%s: could not parse the realm address: %w", d.host, err)
	}

	q := u.Query()
	if svc := params["service"]; svc != "" {
		q.Set("service", svc)
	}
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + repo + ":pull"
	}
	q.Set("scope", scope)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ldi/"+Version)

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return "", fmt.Errorf("%s: could not obtain a token (HTTP %d)%s", d.host, resp.StatusCode, ociDetail(body))
	}

	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("%s: could not read the token response: %w", d.host, err)
	}

	switch {
	case body.Token != "":
		return body.Token, nil
	case body.AccessToken != "":
		return body.AccessToken, nil
	}
	return "", fmt.Errorf("%s: the token response was empty", d.host)
}

// statusError carries the HTTP status alongside the message, so a caller can
// react to the code instead of parsing prose.
type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string { return e.msg }

// isBadRequest reports whether err came back as HTTP 400.
func isBadRequest(err error) bool {
	var se *statusError
	return errors.As(err, &se) && se.code == http.StatusBadRequest
}

// statusError turns the registry's status code into a message worth reading.
func (d *distribution) statusError(code int, body []byte) error {
	return &statusError{code: code, msg: d.statusMessage(code, body)}
}

func (d *distribution) statusMessage(code int, body []byte) string {
	switch code {
	case http.StatusUnauthorized:
		return fmt.Sprintf("%s: authentication required; this registry is closed to anonymous access%s", d.host, ociDetail(body))
	case http.StatusForbidden:
		return fmt.Sprintf("%s: access denied; a subscription or entitlement may be required%s", d.host, ociDetail(body))
	case http.StatusNotFound:
		// Registries deliberately conflate "does not exist" with "you cannot
		// see it", to avoid leaking; the message says so.
		return fmt.Sprintf("%s: repository not found (it may not exist, or you may not be allowed to see it)%s", d.host, ociDetail(body))
	case http.StatusTooManyRequests:
		return fmt.Sprintf("%s: rate limit exceeded%s", d.host, ociDetail(body))
	}
	return fmt.Sprintf("%s: unexpected response (HTTP %d)%s", d.host, code, ociDetail(body))
}

// ociDetail returns the first description in an OCI error body as " - message".
func ociDetail(body []byte) string {
	var e struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &e) != nil || len(e.Errors) == 0 {
		return ""
	}
	first := e.Errors[0]
	if first.Message != "" {
		return " - " + first.Message
	}
	if first.Code != "" {
		return " - " + first.Code
	}
	return ""
}

// parseChallenge splits a WWW-Authenticate header of the form
// `Bearer realm="https://...",service="..."` into its scheme and parameters.
// Parameter names are lowercased.
func parseChallenge(h string) (scheme string, params map[string]string) {
	params = map[string]string{}

	h = strings.TrimSpace(h)
	if h == "" {
		return "", params
	}

	i := strings.IndexAny(h, " \t")
	if i == -1 {
		return h, params
	}
	scheme = h[:i]

	for _, part := range splitOutsideQuotes(h[i+1:], ',') {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(k))
		params[key] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	return scheme, params
}

// nextLink extracts the rel="next" address from an RFC 5988 Link header. A
// registry may return a relative path, so the result is resolved against the
// current address.
func nextLink(header, base string) string {
	for _, part := range splitOutsideQuotes(header, ',') {
		segs := strings.Split(part, ";")
		if len(segs) < 2 {
			continue
		}

		raw := strings.TrimSpace(segs[0])
		if !strings.HasPrefix(raw, "<") || !strings.HasSuffix(raw, ">") {
			continue
		}

		isNext := false
		for _, s := range segs[1:] {
			s = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), " ", ""))
			if s == `rel="next"` || s == "rel=next" {
				isNext = true
				break
			}
		}
		if !isNext {
			continue
		}

		ref, err := url.Parse(strings.Trim(raw, "<>"))
		if err != nil {
			return ""
		}
		if b, err := url.Parse(base); err == nil {
			return b.ResolveReference(ref).String()
		}
		return ref.String()
	}
	return ""
}

// splitOutsideQuotes splits on sep while ignoring separators inside quotes.
func splitOutsideQuotes(s string, sep rune) []string {
	var (
		out     []string
		b       strings.Builder
		inQuote bool
	)
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			b.WriteRune(r)
		case r == sep && !inQuote:
			out = append(out, b.String())
			b.Reset()
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}
