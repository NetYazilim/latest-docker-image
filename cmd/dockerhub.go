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
	"time"
)

// hubTag, hub.docker.com/v2/.../tags yanıtındaki tek kayıt.
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

// httpClient, varsayılan client'ın timeout'suz olmasını önler.
var httpClient = &http.Client{Timeout: 20 * time.Second}

// hubBaseURL, Docker Hub API'sinin kökü. Testler bunu kendi sunucularına
// yöneltebilmek için dockerHub.baseURL alanını değiştirir.
const hubBaseURL = "https://hub.docker.com"

// dockerHub, Docker Hub'ın kendi (tescilli) hub.docker.com/v2 API'sini
// kullanır. Bu API tek çağrıda tag + platform + tarih verdiği için Inspect
// ağa hiç çıkmaz: sayfalama sırasında biriken kayıtları okur.
type dockerHub struct {
	// nameFilter, API'nin sunucu tarafı `name=` substring filtresine gider.
	nameFilter string
	// seen, sayfalama sırasında görülen ham kayıtlar (tag adı -> kayıt).
	seen map[string]hubTag
	// baseURL, API kökü; yalnız testlerde değiştirilir.
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

// NewestFirst: sorgu ordering=last_updated ile yapıldığı için ilk sayfa en
// yeni tag'leri taşır.
func (h *dockerHub) NewestFirst() bool { return true }

func (h *dockerHub) Tags(repo string) TagPager {
	// Docker Hub'da tek parçalı isimler "library" namespace'inde durur.
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
		return TagInfo{}, fmt.Errorf("%s: tag sayfalarda görülmedi", tag)
	}

	info := TagInfo{Tag: t.Name, LastUpdated: t.LastUpdated}
	switch t.ContentType {
	case "plugin":
		// Plugin kayıtları mimari listesi taşımaz.
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

// hubPager, yanıttaki "next" alanını izleyerek sayfaları dolaşır.
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

	// Durum kodu kontrol edilmezse 404/429 gövdesi ({"message": ...}) sorunsuz
	// decode olur, Results boş kalır ve gerçek sebep "Bulunamadı" olarak
	// gizlenir.
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		if msg := apiMessage(body); msg != "" {
			return nil, fmt.Errorf("%s HTTP %d: %s", h.Name(), resp.StatusCode, msg)
		}
		return nil, fmt.Errorf("%s HTTP %d: beklenmeyen yanıt", h.Name(), resp.StatusCode)
	}

	var r hubResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	return &r, nil
}

// apiMessage, Docker Hub hata gövdesindeki açıklamayı çıkarır.
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

// requiredLiteral, re'nin HER eşleşmesinde bulunmak zorunda olan bir literal
// parçayı döndürür (yoksa ""). Sonuç Docker Hub'ın `name=` substring filtresine
// gittiği için yanlış bir literal geçerli tag'leri sessizce gizler; bu yüzden
// sadece zorunlu düğümlere inilir. Alternation ve opsiyonel gruplar atlanır:
// "alpine|bookworm" için "" döner ("alpine" seçilip bookworm tag'lerinin
// kaybedilmesi yerine). En az 2 karakterli literaller dikkate alınır; daha
// kısası anlamlı bir filtre üretmez.
func requiredLiteral(re *syntax.Regexp) string {
	switch re.Op {
	case syntax.OpLiteral:
		// Case-insensitive literal, sunucu tarafı filtreyle uyuşmayabilir.
		if len(re.Rune) < 2 || re.Flags&syntax.FoldCase != 0 {
			return ""
		}
		return string(re.Rune)

	case syntax.OpConcat, syntax.OpCapture, syntax.OpPlus:
		// Bu düğümler her eşleşmede en az bir kez yer alır.
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
