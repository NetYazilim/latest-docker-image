package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// OCI Distribution manifest ortam tipleri. Accept başlığında hepsi birlikte
// istenir: sunucu çok mimarili bir index varsa onu, yoksa tek manifest'i döner.
const (
	mediaOCIIndex       = "application/vnd.oci.image.index.v1+json"
	mediaOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	mediaDockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
)

var manifestAccept = strings.Join([]string{
	mediaOCIIndex, mediaDockerList, mediaOCIManifest, mediaDockerManifest,
}, ", ")

// distribution, OCI Distribution (eski adıyla Docker Registry HTTP API v2)
// konuşan registry'ler için sürücü: registry.redhat.io, registry.access.redhat.com,
// quay.io, ghcr.io, Harbor...
//
// Bu API Docker Hub'ın tescilli API'sinden çok daha az bilgi verir:
// tags/list yalnız isim döner, platform için tag başına manifest çekmek
// gerekir. resolve bu yüzden Inspect'i sadece isim filtresini geçen adaylar
// için çağırıyor.
//
// Faz 1 kapsamı: anonim erişim. Token handshake'i yapılır ama kimlik bilgisi
// gönderilmez; auth.json / config.json okuma Faz 2'de eklenecek.
type distribution struct {
	host string
	// baseURL, registry kökü; yalnız testlerde değiştirilir.
	baseURL string
	// tokens, repo başına Bearer token önbelleği. Token'lar repo kapsamlı ve
	// kısa ömürlü olduğu için süreç ömrü boyunca tutmak yeterli.
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

// NewestFirst: OCI spesifikasyonu tag'lerin sözlük ("ASCIIbetical") sırasında
// dönmesini şart koşar, yani ilk sayfa en yeni sürümü taşımaz. Bu yüzden
// sayfalama erken kesilemez, tüm liste taranmalı.
func (d *distribution) NewestFirst() bool { return false }

func (d *distribution) Tags(repo string) TagPager {
	return &distPager{
		dist: d,
		repo: repo,
		url:  fmt.Sprintf("%s/v2/%s/tags/list?n=100", d.baseURL, repo),
	}
}

// distPager, Link başlığındaki rel="next" bağlantısını izleyerek sayfaları
// dolaşır.
type distPager struct {
	dist *distribution
	repo string
	url  string
	done bool
}

func (p *distPager) Next(ctx context.Context) ([]string, error) {
	if p.done {
		return nil, nil
	}

	resp, err := p.dist.get(ctx, p.repo, p.url, "application/json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var body struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("%s: tag listesi okunamadı: %w", p.dist.host, err)
	}

	if next := nextLink(resp.Header.Get("Link"), p.url); next != "" {
		p.url = next
	} else {
		p.done = true
	}

	// Boş repolarda "tags": null gelebiliyor. nil dönmek "sayfalar bitti"
	// anlamına geldiği için boş dilime çeviriyoruz.
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
		return TagInfo{}, fmt.Errorf("%s: %s manifest'i okunamadı: %w", d.host, tag, err)
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
		return TagInfo{}, fmt.Errorf("%s: %s manifest'i ayrıştırılamadı: %w", d.host, tag, err)
	}

	// Çok mimarili index: platform listesi manifest'in kendisinde.
	if len(m.Manifests) > 0 {
		for _, sub := range m.Manifests {
			// buildkit'in attestation/imza kayıtları "unknown/unknown"
			// platformuyla geliyor; bunlar çalıştırılabilir imaj değil.
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

	// Tek manifest: platform bilgisi manifest'te yok, config blob'unda.
	// Bu yüzden yalnız tek mimarili imajlarda bir ek istek gerekiyor.
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
	// config.created imajın derlenme zamanı; Docker Hub'ın last_updated
	// alanının en yakın karşılığı ve yalnız burada bedava geliyor.
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
		return imageConfig{}, fmt.Errorf("%s: config blob'u okunamadı: %w", d.host, err)
	}
	return cfg, nil
}

// get, isteği yapar; 401 gelirse meydan okumadan token alıp bir kez tekrar
// dener. Dönen yanıtın gövdesini çağıran kapatır.
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

// fetchToken, 401 yanıtındaki WWW-Authenticate meydan okumasından realm ve
// service değerlerini okuyup token ister. Realm sabit olarak gömülmez; her
// registry kendi adresini bu başlıkta bildirir.
//
// Faz 2: kimlik bilgisi (Registry Service Account) burada req.SetBasicAuth ile
// eklenecek. Şu hali anonim token alır, bu da registry.access.redhat.com,
// quay.io ve ghcr.io'nun public içeriği için yeterli.
func (d *distribution) fetchToken(ctx context.Context, repo, challenge string) (string, error) {
	scheme, params := parseChallenge(challenge)
	if scheme == "" {
		return "", fmt.Errorf("%s: 401 yanıtında WWW-Authenticate başlığı yok", d.host)
	}
	if !strings.EqualFold(scheme, "bearer") {
		return "", fmt.Errorf("%s: desteklenmeyen kimlik doğrulama yöntemi %q", d.host, scheme)
	}

	realm := params["realm"]
	if realm == "" {
		return "", fmt.Errorf("%s: kimlik doğrulama meydan okumasında realm yok", d.host)
	}

	u, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("%s: realm adresi çözümlenemedi: %w", d.host, err)
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
		return "", fmt.Errorf("%s: token alınamadı (HTTP %d)%s", d.host, resp.StatusCode, ociDetail(body))
	}

	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return "", fmt.Errorf("%s: token yanıtı okunamadı: %w", d.host, err)
	}

	switch {
	case body.Token != "":
		return body.Token, nil
	case body.AccessToken != "":
		return body.AccessToken, nil
	}
	return "", fmt.Errorf("%s: token yanıtı boş", d.host)
}

// statusError, registry'nin durum kodunu işe yarar bir mesaja çevirir.
func (d *distribution) statusError(code int, body []byte) error {
	switch code {
	case http.StatusUnauthorized:
		return fmt.Errorf("%s: kimlik doğrulama gerekiyor; bu registry anonim erişime kapalı%s", d.host, ociDetail(body))
	case http.StatusForbidden:
		return fmt.Errorf("%s: erişim yok; abonelik/entitlement gerekebilir%s", d.host, ociDetail(body))
	case http.StatusNotFound:
		// Registry'ler sızıntıyı önlemek için "yok" ile "göremiyorsun"u
		// ayırmaz; mesaj bunu söylüyor.
		return fmt.Errorf("%s: repo bulunamadı (ya yok ya da görme yetkiniz yok)%s", d.host, ociDetail(body))
	case http.StatusTooManyRequests:
		return fmt.Errorf("%s: istek limiti aşıldı%s", d.host, ociDetail(body))
	}
	return fmt.Errorf("%s: beklenmeyen yanıt (HTTP %d)%s", d.host, code, ociDetail(body))
}

// ociDetail, OCI hata gövdesindeki ilk açıklamayı " - mesaj" biçiminde döndürür.
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

// parseChallenge, `Bearer realm="https://...",service="..."` biçimindeki
// WWW-Authenticate başlığını şema ve parametrelerine ayırır. Parametre
// adları küçük harfe indirilir.
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

// nextLink, RFC 5988 Link başlığından rel="next" adresini çıkarır. Registry'ler
// göreli yol döndürebildiği için sonuç mevcut adrese göre çözümlenir.
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

// splitOutsideQuotes, tırnak içindeki ayırıcıları yok sayarak böler.
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
