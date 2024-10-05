package main

import (
	"encoding/json"
	"fmt"
	"github.com/cristalhq/aconfig"
	"github.com/cristalhq/aconfig/aconfigdotenv"
	"golang.org/x/mod/semver"
	"log"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"sort"
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
	Count   int    `json:"count"`
	Next    string `json:"next"`
	Results []Tag  `json:"results"`
}

// uppercase all char "architecture"

type Config struct {
	Architecture string `flag:"arch" env:"arch"  default:""`
	OS           string `flag:"os"   env:"os"`
	Tag          string `flag:"tag"  env:"tag"`
}

// ByVersion implements sort.Interface for sorting semantic version strings.
type ByVersion []string

var cfg Config
var Version = "1.2.0"
var response Response
var msg string
var filteredTags []string

func main() {

	repo := ""
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
	flags := loader.Flags()
	if err := flags.Parse(os.Args[1:]); err != nil {
		log.Fatalf("Error: ", err)
	}

	if len(flags.Args()) == 0 {
		fmt.Fprintf(os.Stderr, `
_   __   _  
|   | \  |
|__ |_/  |  v%s
Latest Docker Image    
`, Version)

		fmt.Fprintln(os.Stderr, `
No repository not specified
Usage:
 ldi [-arch <Architecture>] [-os <Operating Systetem>]  [-tag <tag regex filter>] <Repository>    

example:
 ldi  -tag '^(\d+)\.(\d+)\.(\d+)$'  grafana/grafana-oss
`)

		os.Exit(-2)
	}
	repo = flags.Args()[0]

	if err := loader.Load(); err != nil {
		log.Fatalf("failed to load configuration: %v", err)
	}

	if cfg.Architecture == "" {
		cfg.Architecture = runtime.GOARCH
	}

	if !strings.Contains(repo, "/") {
		repo = "library/" + repo
	}

	url := fmt.Sprintf("https://hub.docker.com/v2/repositories/%s/tags?page_size=100&page=1&ordering=last_updated", repo)

	resp, err := http.Get(url)
	if err != nil {
		log.Fatalf("HTTP request failed: %v", err)
	}
	defer resp.Body.Close()

	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		log.Fatalf("Failed to decode JSON response: %v", err)
	}

	re := regexp.MustCompile(cfg.Tag)

	Count := 0
	found := false
	for !found {

		for _, tag := range response.Results {
			Count = Count + 1
			if !re.MatchString(tag.Name) {
				continue
			}
			if regexp.MustCompile(`beta|rc|latest`).MatchString(tag.Name) {
				continue
			}

			// Mimari kontrolü
			architectureMatch := false
			statusMatch := false
			for _, detail := range tag.Images {
				//		fmt.Printf("  Architecture: %s,  Status: %s", detail.Architecture, detail.Status)

				if regexp.MustCompile(cfg.Architecture).MatchString(detail.Architecture) {
					architectureMatch = true

					if detail.Status == "active" {
						statusMatch = true

						break
					}
				}
			}

			if !architectureMatch {
				msg = fmt.Sprintf("missing %s architecture", cfg.Architecture)
				continue
			}
			// Active kontrolü

			if !statusMatch {
				msg = fmt.Sprintf("status: inactive")
				continue
			}

			filteredTags = append(filteredTags, tag.Name)
		}

		if len(filteredTags) > 0 {
			found = true
		}
		if Count >= response.Count {
			break
		}

		url := response.Next
		resp, err := http.Get(url)
		if err != nil {
			log.Fatalf("HTTP request failed: %v", err)
		}
		defer resp.Body.Close()

		if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
			log.Fatalf("Failed to decode JSON response: %v", err)
		}

	}

	// En güncel etiketi al (sonuncu)
	fmt.Fprintf(os.Stderr, "Repository: %s, Architecture: %s, Tag Filter: %s", repo, cfg.Architecture, cfg.Tag)
	if len(filteredTags) > 0 {

		sort.Slice(filteredTags, func(i, j int) bool {
			v1 := filteredTags[i]
			v2 := filteredTags[j]
			if v1[0] != 'v' {
				v1 = "v" + v1
			}
			if v2[0] != 'v' {
				v2 = "v" + v2
			}

			return semver.Compare(v1, v2) > 0
		})

		//fmt.Printf("%s:%s", cfg.Repository, latestTag)
		fmt.Fprintf(os.Stderr, ", Lates Tag: %s\n", filteredTags[0])
		fmt.Fprintf(os.Stdout, "%s:%s", flags.Args()[0], filteredTags[0])

	} else {
		fmt.Fprintf(os.Stderr, ", No found %s\n", msg)
		os.Exit(-1)
	}

}

// env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o ./bin/latest-docker-image ./cmd
// env CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags="-w -s" -o ./bin/latest-docker-image.exe ./cmd
