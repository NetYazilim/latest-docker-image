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
	"runtime"
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
	Architecture string `flag:"arch" env:"arch"  default:""`
	OS           string `flag:"os"   env:"os"`
	Tag          string `flag:"tag"  env:"tag"`
}

var cfg Config
var Version = "1.1.0"

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
|__ |_/  |  %s
Latest Docker Image    
`, "v"+Version)

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
	var msg string
	var latestTag string

	for _, tag := range response.Results {

		if !re.MatchString(tag.Name) {
			continue
		}
		if regexp.MustCompile(`beta|rc|latest`).MatchString(tag.Name) {
			continue
		}
		fmt.Printf("Tag: %s ", tag.Name)
		// Mimari kontrolü
		architectureMatch := false
		statusMatch := false
		for _, detail := range tag.Images {
			fmt.Printf("  Architecture: %s,  Status: %s", detail.Architecture, detail.Status)

			if regexp.MustCompile(cfg.Architecture).MatchString(detail.Architecture) {
				architectureMatch = true
				fmt.Printf("  architectureMatch: %v", architectureMatch)
				if detail.Status == "active" {
					statusMatch = true
					fmt.Printf("  statusMatch: %v\n", statusMatch)
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

		latestTag = tag.Name
		break

	}

	// En güncel etiketi al (sonuncu)
	fmt.Fprintf(os.Stderr, "Repository: %s, Architecture: %s, Tag Filter: %s", repo, cfg.Architecture, cfg.Tag)
	if latestTag != "" {

		//fmt.Printf("%s:%s", cfg.Repository, latestTag)
		fmt.Fprintf(os.Stderr, ", Lates Tag: %s\n", latestTag)
		fmt.Fprintf(os.Stdout, "%s:%s", repo, latestTag)

	} else {
		fmt.Fprintf(os.Stderr, ", No found %s\n", msg)
		os.Exit(-1)
	}
}

// env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o ./bin/latest-docker-image ./cmd
// env CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags="-w -s" -o ./bin/latest-docker-image.exe ./cmd
