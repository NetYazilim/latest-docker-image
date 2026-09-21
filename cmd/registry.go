package main

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

// httpClient, varsayılan client'ın timeout'suz olmasını önler. Sürücüler
// bağlantıyı yeniden kullanabilmek için aynı client'ı paylaşır.
var httpClient = &http.Client{Timeout: 20 * time.Second}

// Platform, bir imajın yayınlandığı işletim sistemi/mimari çifti.
type Platform struct {
	Arch string
	OS   string
}

// TagInfo, tek bir tag hakkında sürücünün bildiklerini taşır.
type TagInfo struct {
	Tag string

	// Platforms, tag'in yayınlandığı platformlar. Boş liste "platform bilgisi
	// yok" demektir ve platform filtresi böyle bir tag'i geçirmez.
	Platforms []Platform

	// AnyPlatform, platform filtresinin bu tag'e uygulanmaması gerektiğini
	// bildirir. Docker Hub'da content_type == "plugin" olan kayıtlar mimari
	// listesi taşımaz ve bu şekilde işaretlenir.
	AnyPlatform bool

	// LastUpdated, sürücünün verdiği ham zaman damgası. Her registry bu bilgiyi
	// ucuza veremez, dolayısıyla boş olabilir: OCI Distribution'da çok mimarili
	// bir index için hiç gelmez.
	LastUpdated string

	// Digest, tag'in işaret ettiği manifest digest'i. Tarih yoksa çıktıda onun
	// yerine bu gösterilir; güncelleme kontrolü için de doğru alan budur.
	Digest string
}

// TagPager, bir reponun tag isimlerini sayfa sayfa okur.
type TagPager interface {
	// Next bir sonraki sayfayı döndürür; sayfalar tükendiğinde (nil, nil).
	// Boş ama nil olmayan bir dilim "bu sayfa boş, devam et" anlamındadır.
	Next(ctx context.Context) ([]string, error)
}

// Registry, bir repodaki tag'lerin kaynağı. Faz 1'de OCI Distribution sürücüsü
// de bu arayüzü uygulayacak.
type Registry interface {
	// Name, hata mesajlarında görünen registry adı.
	Name() string

	// Tags, repo için tag isimlerini sayfalayan bir okuyucu döndürür.
	Tags(repo string) TagPager

	// Inspect, tek bir tag'in platform ve tarih bilgisini verir. Bu çağrı ağa
	// çıkabilir; resolve onu yalnız isim filtresini geçen tag'ler için yapar.
	Inspect(ctx context.Context, repo, tag string) (TagInfo, error)

	// NewestFirst, Tags'in en yeni tag'i ilk sayfada verdiğini bildirir. true
	// ise resolve, eşleşme bulduğu sayfada sayfalamayı bırakır. OCI
	// Distribution sözlük sırası kullandığı için orada false olacak.
	NewestFirst() bool
}

// ErrNoMatch, filtreleri geçen hiçbir tag bulunamadığında döner.
var ErrNoMatch = errors.New("eşleşen tag bulunamadı")

// excludeRe, ön-sürüm ve kayan (floating) tag'leri eler. Kalıp sınırlara
// bağlıdır: sınırsız `rc` deseni "torch", "arch", "source" gibi geçerli
// tag'leri de sessizce düşürüyordu.
//
// "-source" ayrıca elenir: Red Hat registry'lerinde her imajın yanında bir
// kaynak konteyneri yayınlanıyor (ör. "9.0.0-1468-source") ve bunlar
// çalıştırılabilir imaj değil. Sınırsız `rc` deseni bunları tesadüfen
// eliyordu ("source" içinde "rc" geçiyor); artık açıkça belirtiliyor.
var excludeRe = regexp.MustCompile(
	`(?i)(^|[-._])(alpha|beta|rc|pre|preview|dev|snapshot|nightly|canary|edge)([-._0-9]|$)` +
		`|^latest([-._]|$)` +
		`|[-._]source$`)

// resolve, ortak seçim hattıdır: isimleri sayfala, isim üzerinden filtrele,
// yalnız filtreyi geçenlerin platformunu sorgula, sırala ve en uygun tag'i
// döndür. Platform sorgusunun isim filtresinden SONRA gelmesi kasıtlı: ağa
// çıkma maliyeti yüksek olan sürücülerde (Distribution) istek sayısını
// aday sayısına indirir.
func resolve(ctx context.Context, reg Registry, repo string, filter *regexp.Regexp, arch, osName string) (TagInfo, error) {
	pager := reg.Tags(repo)

	var matches []TagInfo
	for {
		names, err := pager.Next(ctx)
		if err != nil {
			return TagInfo{}, err
		}
		if names == nil {
			break
		}

		for _, name := range names {
			if !filter.MatchString(name) || excludeRe.MatchString(name) {
				continue
			}

			info, err := reg.Inspect(ctx, repo, name)
			if err != nil {
				return TagInfo{}, err
			}
			if platformMatches(info, arch, osName) {
				matches = append(matches, info)
			}
		}

		// Sayfalar en yeniden eskiye geliyorsa eşleşme bulunduğu anda durmak
		// güvenli; sözlük sırasıyla gelen registry'lerde yanlış olur. Kararı
		// bu yüzden sürücü veriyor.
		if len(matches) > 0 && reg.NewestFirst() {
			break
		}
	}

	if len(matches) == 0 {
		return TagInfo{}, ErrNoMatch
	}

	sortTags(matches)

	best := matches[0]
	if len(matches) > 1 {
		next := matches[1]
		// "1.0" ile "1.0.1" gibi durumlarda daha özgül olanı tercih et.
		// NOT: semver sıralamasıyla birleştiğinde bu kural "1.2.3" yerine
		// "1.2.3-alpine" seçilmesine yol açabiliyor. Davranış bilinçli olarak
		// korunuyor; README regex'i `$` ile bağlamayı öneriyor.
		if strings.HasPrefix(next.Tag, best.Tag) && len(next.Tag) > len(best.Tag) {
			best = next
		}
	}
	return best, nil
}

// platformMatches, tag'in istenen os/mimari için yayınlanıp yayınlanmadığını
// söyler.
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

// sortTags, tag'leri yeniden eskiye (en yüksek sürüm başta) sıralar.
func sortTags(tags []TagInfo) {
	slices.SortFunc(tags, func(a, b TagInfo) int {
		return compareTags(a.Tag, b.Tag)
	})
}

// compareTags, a'nın b'den önce gelmesi gerekiyorsa negatif döner. Aynı temel
// sürümün -security- yeniden derlemesi düz sürümden önce gelir.
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

// Faz 1 için notlar (quay.io, OCI Distribution):
// https://quay.io/api/v1/repository/prometheus/node-exporter/tag/?limit=100&page=1&onlyActiveTags=true
// https://quay.io/api/v1/repository/prometheus/node-exporter/manifest/sha256:...
// https://github.com/shogo82148/docker-image-update-checker/blob/main/registry/registry.go
