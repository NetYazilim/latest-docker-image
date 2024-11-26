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
	"time"
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

var cfg Config
var Version = "1.3.0"
var response Response
var msg string

var result []Result

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
Repository name not specified
Usage:  ldi  [OPTIONS] IMAGE[:TAG]    
Show information about the latest version of a Docker IMAGE in the Docker Hub.

Options:
  -arch string    Architecture
  -os string      Operating Systetem, default: linux

TAG filter options:
  emty for latest tag
  regular expression for tag filter
  @DIGEST for a specific digest

example:
 ldi grafana/grafana-oss
 ldi grafana/grafana-oss:'(\d+)\.(\d+)\.(\d+)'
 ldi portainer/portainer-ee:'(\d+)\.(\d+)\.(\d+)-alpine$'
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

	if cfg.OS == "" {
		cfg.Architecture = "linux"
	}

	if strings.Contains(repo, ":") {
		cfg.Tag = strings.Split(repo, ":")[1]
		repo = strings.Split(repo, ":")[0]
	}
	repou := repo
	if !strings.Contains(repo, "/") {
		repou = "library/" + repo
	}

	url := fmt.Sprintf("https://hub.docker.com/v2/repositories/%s/tags?page_size=100&page=1&ordering=last_updated", repou)

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
			OSMatch := false
			statusMatch := false

			switch {
			case tag.ContentType == "plugin":
				result = append(result, Result{Tag: tag.Name, LastUpdated: tag.LastUpdated})

			case tag.ContentType == "image":
				for _, detail := range tag.Images {
					//		fmt.Printf("  Architecture: %s,  OS: %s, Status: %s\n", detail.Architecture, detail.OS, detail.Status)

					if cfg.Architecture == detail.Architecture {
						architectureMatch = true

						if cfg.OS == detail.OS {
							OSMatch = true

							if detail.Status == "active" {
								statusMatch = true
								break
							}
						}
					}
				}

				if !OSMatch {
					msg = fmt.Sprintf("missing image for %s OS", cfg.OS)
					continue
				}

				if !architectureMatch {
					msg = fmt.Sprintf("missing image for %s architecture", cfg.Architecture)
					continue
				}

				if !statusMatch {
					msg = fmt.Sprintf("status: inactive")
					continue
				}

				result = append(result, Result{Tag: tag.Name, LastUpdated: tag.LastUpdated})
			default:
				fmt.Fprintf(os.Stderr, ", Unsupported ContentType (%s)\n", tag.ContentType)
				os.Exit(-1)
			}

		}

		if len(result) > 0 {
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

	fmt.Fprintf(os.Stderr, "\nRepo.: %s, Arch.: %s, OS: %s, Filter: %s", repo, cfg.Architecture, cfg.OS, cfg.Tag)

	if len(result) == 0 {
		fmt.Fprintf(os.Stderr, ", No found %s\n", msg)
		os.Exit(-1)

	}

	sort.Slice(result, func(i, j int) bool {
		v1 := result[i].Tag
		v2 := result[j].Tag
		if v1[0] != 'v' {
			v1 = "v" + v1
		}
		if v2[0] != 'v' {
			v2 = "v" + v2
		}

		return semver.Compare(v1, v2) > 0
	})
	lastUpdated := result[0].LastUpdated
	t, err := time.Parse(time.RFC3339Nano, lastUpdated)
	if err == nil {
		lastUpdated = t.Format("2006-01-02 15:04:05Z")
	}

	fmt.Fprintf(os.Stderr, ", Tag: %s,  Update: %s\n", result[0].Tag, lastUpdated)
	fmt.Fprintf(os.Stdout, "%s:%s", repo, result[0].Tag)

}

// env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o ./bin/latest-docker-image ./cmd
// env CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags="-w -s" -o ./bin/latest-docker-image.exe ./cmd
