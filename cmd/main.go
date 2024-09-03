package main

import (
	"encoding/json"
	"fmt"
	"github.com/cristalhq/aconfig"
	"github.com/cristalhq/aconfig/aconfigdotenv"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
)

type Tag struct {
	Name string `json:"name"`

	Images []TagDetail `json:"images"`
}

type TagDetail struct {
	Architecture string `json:"architecture"`
	Status       string `json:"status"`
}

type Response struct {
	Results []Tag `json:"results"`
}

// uppercase all char "architecture"

type Config struct {
	Repository   string `flag:"repo" env:"repo"`
	Architecture string `flag:"arch" env:"arch"  default:"amd64"`
	OS           string `flag:"os"   env:"os"`
	Tag          string `flag:"tag"  env:"tag"`
}

var cfg Config
var Version = "1.0.0"

func main() {

	fmt.Fprintf(os.Stderr, `
_   __   _  
|   | \  |
|__ |_/  |
Latest Docker Image %s    
`, "v"+Version)

	loader := aconfig.LoaderFor(&cfg, aconfig.Config{
		SkipFlags:          false,
		AllowUnknownFlags:  false,
		AllowUnknownEnvs:   false,
		AllowUnknownFields: true,
		SkipEnv:            false,
		MergeFiles:         true,
		FileDecoders: map[string]aconfig.FileDecoder{
			".env": aconfigdotenv.New(),
		},
		Files: []string{".env", ".env"},
	})
	if err := loader.Load(); err != nil {
		log.Fatalf("failed to load configuration: %v", err)
	}

	repo := cfg.Repository
	if !strings.Contains(repo, "/") {
		repo = "library/" + repo
	}

	url := fmt.Sprintf("https://hub.docker.com/v2/repositories/%s/tags?page_size=250&page=1&ordering=last_updated", repo)

	resp, err := http.Get(url)
	if err != nil {
		log.Fatalf("HTTP request failed: %v", err)
	}
	defer resp.Body.Close()

	var response Response
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		log.Fatalf("Failed to decode JSON response: %v", err)
	}

	// Regex deseni tanımla
	//re := regexp.MustCompile(`-alpine$`)
	re := regexp.MustCompile(cfg.Tag)

	var filteredTags []string
	for _, tag := range response.Results {
		if !re.MatchString(tag.Name) {
			continue
		}
		if regexp.MustCompile(`beta|rc|latest`).MatchString(tag.Name) {
			continue
		}

		// Mimari kontrolü
		architectureMatch := false
		for _, detail := range tag.Images {
			if regexp.MustCompile(cfg.Architecture).MatchString(detail.Architecture) {
				architectureMatch = true
				break
			}
		}
		if !architectureMatch {
			continue
		}

		// Active kontrolü
		statusMatch := false
		for _, detail := range tag.Images {
			if detail.Status == "active" {
				statusMatch = true
				break
			}
		}
		if !statusMatch {
			continue
		}
		filteredTags = append(filteredTags, tag.Name)
	}

	// Etiketleri sıralama
	//	sort.Strings(filteredTags)

	// En güncel etiketi al (sonuncu)
	if len(filteredTags) > 0 {
		latestTag := filteredTags[len(filteredTags)-1]
		//fmt.Printf("%s:%s", cfg.Repository, latestTag)
		fmt.Fprintf(os.Stderr, "Repository: %s, Architecture: %s, Tag Filter: %s, Lates Tag: %s\n", cfg.Repository, cfg.Architecture, cfg.Tag, latestTag)
		fmt.Fprintf(os.Stdout, "%s:%s", cfg.Repository, latestTag)

	} else {
		os.Exit(1)
	}
}

// env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o ./bin/latest-docker-image ./cmd
// env CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags="-w -s" -o ./bin/latest-docker-image.exe ./cmd
