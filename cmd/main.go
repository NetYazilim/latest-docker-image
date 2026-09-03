package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"regexp/syntax"
	"runtime"
	"slices"
	"strings"
	"time"

	cli "github.com/urfave/cli/v3"
	"golang.org/x/mod/semver"
)

type Tag struct {
	Name        string      `json:"name"`
	ContentType string      `json:"content_type"`
	LastUpdated string      `json:"last_updated"`
	Images      []TagDetail `json:"images"`
}

type TagDetail struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Status       string `json:"status"`
}

type Response struct {
	Count   int    `json:"count"`
	Next    string `json:"next"`
	Results []Tag  `json:"results"`
}

// uppercase all char "architecture"

type Config struct {
	Architecture string `flag:"arch" env:"arch" default:""`
	OS           string `flag:"os"   env:"os"   default:"linux"`
	Tag          string `flag:"tag"  env:"tag"`
}

type Result struct {
	Tag         string `flag:"tag"  env:"tag"`
	LastUpdated string `json:"last_updated"`
}

// ByVersion implements sort.Interface for sorting semantic version strings.
type ByVersion []string

var (
	cfg     Config
	Version = "1.5.2"
)

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

// httpClient, varsayılan client'ın timeout'suz olmasını önler.
var httpClient = &http.Client{Timeout: 20 * time.Second}

func fetchTags(ctx context.Context, repoURL string) (*Response, error) {
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
	// decode olur, Results boş kalır ve gerçek sebep "Bulunamadı" olarak gizlenir.
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		if msg := apiMessage(body); msg != "" {
			return nil, fmt.Errorf("docker hub HTTP %d: %s", resp.StatusCode, msg)
		}
		return nil, fmt.Errorf("docker hub HTTP %d: beklenmeyen yanıt", resp.StatusCode)
	}

	var r Response
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

// excludeRe, ön-sürüm ve kayan (floating) tag'leri eler. Kalıp sınırlara
// bağlıdır: sınırsız `rc` deseni "torch", "arch", "source" gibi geçerli
// tag'leri de sessizce düşürüyordu.
var excludeRe = regexp.MustCompile(
	`(?i)(^|[-._])(alpha|beta|rc|pre|preview|dev|snapshot|nightly|canary|edge)([-._0-9]|$)` +
		`|^latest([-._]|$)`)

func filterTags(tags []Tag, re *regexp.Regexp, arch, osName string) []Result {
	var filtered []Result
	for _, tag := range tags {
		if !re.MatchString(tag.Name) {
			continue
		}
		if excludeRe.MatchString(tag.Name) {
			continue
		}

		switch tag.ContentType {
		case "plugin":
			filtered = append(filtered, Result{Tag: tag.Name, LastUpdated: tag.LastUpdated})
		case "image":
			match := false
			for _, detail := range tag.Images {
				if arch == detail.Architecture && osName == detail.OS && detail.Status == "active" {
					match = true
					break
				}
			}
			if match {
				filtered = append(filtered, Result{Tag: tag.Name, LastUpdated: tag.LastUpdated})
			}
		}
	}
	return filtered
}

func sortResults(results []Result) {
	slices.SortFunc(results, func(a, b Result) int {
		v1 := a.Tag
		if !strings.HasPrefix(v1, "v") {
			v1 = "v" + v1
		}
		v2 := b.Tag
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
	})
}

func main() {
	cmd := &cli.Command{
		Name:    "ldi",
		Usage:   "Show information about the latest version of a Docker IMAGE in the Docker Hub.",
		Version: Version,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:        "arch",
				Usage:       "Architecture",
				Value:       runtime.GOARCH,
				Destination: &cfg.Architecture,
			},
			&cli.StringFlag{
				Name:        "os",
				Usage:       "Operating System",
				Value:       "linux",
				Destination: &cfg.OS,
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.Args().Len() == 0 {
				cli.ShowAppHelp(cmd)
				return nil
			}

			repo := cmd.Args().First()

			if strings.Contains(repo, ":") {
				cfg.Tag = strings.Split(repo, ":")[1]
				repo = strings.Split(repo, ":")[0]
			}

			repou := repo
			if !strings.Contains(repo, "/") {
				repou = "library/" + repo
			}

			re, err := regexp.Compile(cfg.Tag)
			if err != nil {
				return fmt.Errorf("invalid tag regex: %w", err)
			}

			name := ""
			if rep, perr := syntax.Parse(cfg.Tag, syntax.Perl); perr == nil {
				name = requiredLiteral(rep.Simplify())
			}

			q := url.Values{
				"page_size": {"100"},
				"status":    {"active"},
				"page":      {"1"},
				"ordering":  {"last_updated"},
			}
			if name != "" {
				q.Set("name", name)
			}
			apiURL := fmt.Sprintf("https://hub.docker.com/v2/repositories/%s/tags?%s", repou, q.Encode())

			var allResults []Result
			for {
				resp, err := fetchTags(ctx, apiURL)
				if err != nil {
					return fmt.Errorf("HTTP isteği başarısız: %w", err)
				}

				allResults = append(allResults, filterTags(resp.Results, re, cfg.Architecture, cfg.OS)...)

				if len(allResults) > 0 || resp.Next == "" {
					break
				}
				apiURL = resp.Next
			}

			fmt.Fprintf(os.Stderr, "\nRepo.: %s, Arch.: %s, OS: %s, Filter: %s", repo, cfg.Architecture, cfg.OS, cfg.Tag)

			// Hata durumunda stdout BOŞ kalmalı: README'deki
			// `docker pull $(ldi ...)` kalıbı aksi halde
			// "docker pull no-new-image!" komutunu çalıştırıyordu.
			// (os.Exit(-1) de gerçekte 255 kodunu üretiyordu.)
			if len(allResults) == 0 {
				fmt.Fprintf(os.Stderr, ", Bulunamadı\n")
				os.Exit(1)
			}

			sortResults(allResults)

			lastTag := allResults[0].Tag
			lastUpdated := allResults[0].LastUpdated

			if len(allResults) > 1 {
				// Eğer bir sonraki tag, mevcut tag ile başlıyorsa ve daha uzunsa (örn: "1.0" ve "1.0.1")
				// Onu tercih et (bu mantık Docker Hub'ın bazı tagleme alışkanlıkları için eklenmiş olabilir)
				if strings.HasPrefix(allResults[1].Tag, lastTag) && len(allResults[1].Tag) > len(lastTag) {
					lastTag = allResults[1].Tag
					lastUpdated = allResults[1].LastUpdated
				}
			}

			t, err := time.Parse(time.RFC3339Nano, lastUpdated)
			if err == nil {
				lastUpdated = t.Format("2006-01-02 15:04:05Z")
			}

			fmt.Fprintf(os.Stderr, ", Tag: %s,  Update: %s\n", lastTag, lastUpdated)
			fmt.Fprintf(os.Stdout, "%s:%s", repo, lastTag)
			return nil
		},
	}

	if err := cmd.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "ldi:", err)
		os.Exit(1)
	}
}

// env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o ./bin/latest-docker-image ./cmd
// env CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags="-w -s" -o ./bin/latest-docker-image.exe ./cmd

// https://quay.io/api/v1/repository/prometheus/node-exporter/tag/?limit=100&page=1&onlyActiveTags=true  > tag
// https://quay.io/api/v1/repository/prometheus/node-exporter/manifest/sha256:fa7fa12a57eff607176d5c363d8bb08dfbf636b36ac3cb5613a202f3c61a6631 >arch

//TODO: "github.com/urfave/cli/v3" ile yap
// https://github.com/shogo82148/docker-image-update-checker/blob/main/registry/registry.go
