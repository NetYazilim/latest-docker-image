package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"regexp/syntax"
	"runtime"
	"time"

	cli "github.com/urfave/cli/v3"
)

// Config holds the settings that come from the command line.
type Config struct {
	Architecture string
	OS           string
	Tag          string
	Verbose      bool
}

var (
	cfg Config
	// Version is stamped in at link time by build.sh
	// (-ldflags "-X main.Version=..."). A plain `go build` leaves it at
	// "dev": it has no way to know a release number.
	Version = "dev"
)

func main() {
	cmd := &cli.Command{
		Name:    "ldi",
		Usage:   "Show information about the latest version of a container IMAGE in Docker Hub or in any registry speaking OCI Distribution.",
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
			&cli.BoolFlag{
				// Not "v": urfave already uses that as the alias of --version.
				Name:        "verbose",
				Aliases:     []string{"V"},
				Usage:       "Report what the lookup cost",
				Destination: &cfg.Verbose,
			},
		},
		Action: run,
	}

	if err := cmd.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "ldi:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cmd *cli.Command) error {
	if cmd.Args().Len() == 0 {
		cli.ShowAppHelp(cmd)
		return nil
	}

	ref, err := ParseReference(cmd.Args().First())
	if err != nil {
		return err
	}
	if ref.Digest != "" {
		return fmt.Errorf("a pinned @digest reference is not supported: %s", ref.Digest)
	}
	repo := ref.Repo
	cfg.Tag = ref.Filter
	// name is the full name shown to the user and written to stdout (host
	// included); repo is the path asked of the registry.
	name := ref.Name()

	filter, err := regexp.Compile(cfg.Tag)
	if err != nil {
		return fmt.Errorf("invalid tag regex: %w", err)
	}

	reg := pickRegistry(ref)

	// Printed before the lookup, not after: on a registry with a large tag list
	// resolve can take a while, and a silent terminal looks like a hung tool.
	fmt.Fprintf(os.Stderr, "\nRepo.: %s, Arch.: %s, OS: %s, Filter: %s", name, cfg.Architecture, cfg.OS, cfg.Tag)

	start := time.Now()
	info, st, err := resolve(ctx, reg, repo, filter, cfg.Architecture, cfg.OS)
	spent := time.Since(start)

	if err != nil && !errors.Is(err, ErrNoMatch) {
		// No blanket wrapper here: every driver already names itself in its
		// errors, and a generic "request failed" prefix made an
		// authentication failure read like a network problem.
		fmt.Fprintln(os.Stderr)
		reportStats(st, spent)
		return err
	}

	// On failure stdout must stay EMPTY: otherwise the README's
	// `docker pull $(ldi ...)` idiom ended up running
	// "docker pull no-new-image!".
	// (os.Exit(-1) also produced 255 rather than 1.)
	if errors.Is(err, ErrNoMatch) {
		fmt.Fprintf(os.Stderr, ", Not found\n")
		reportStats(st, spent)
		os.Exit(1)
	}

	reportTag(info)
	reportStats(st, spent)
	fmt.Fprintf(os.Stdout, "%s:%s", name, info.Tag)
	return nil
}

// pickRegistry chooses the driver for a reference.
//
// Docker Hub speaks the Distribution protocol too, over registry-1.docker.io,
// but moving it there would make no sense: its proprietary API answers with
// tag, platform and date in one call, while the same information would cost two
// extra requests per tag and count against Hub's pull limit.
func pickRegistry(ref Reference) Registry {
	if ref.Host != "" && !isDockerHub(ref.Host) {
		return newDistribution(ref.Host)
	}

	// When the regex has a required literal part, it is handed to Docker Hub's
	// server-side `name=` filter to cut down the number of pages.
	// Distribution has no such filter.
	nameFilter := ""
	if rep, err := syntax.Parse(ref.Filter, syntax.Perl); err == nil {
		nameFilter = requiredLiteral(rep.Simplify())
	}
	return newDockerHub(nameFilter)
}

// isDockerHub recognises the host aliases that name Docker Hub.
func isDockerHub(host string) bool {
	switch host {
	case "docker.io", "index.docker.io", "registry-1.docker.io", "registry.hub.docker.com":
		return true
	}
	return false
}

// reportStats explains what the lookup cost, under -verbose. It prints on the
// failure paths too: a lookup that found nothing, or gave up at the cap, is
// precisely when the numbers are worth having.
//
// Page and lookup counts are what separate a slow registry from a slow
// strategy. gcr.io answers tags/list with a single response of about 14 MB;
// public.ecr.aws paginates a far longer list; Docker Hub stops after the first
// page that matches. Before this flag, telling those apart was guesswork.
func reportStats(st lookupStats, spent time.Duration) {
	if !cfg.Verbose {
		return
	}

	fmt.Fprintf(os.Stderr, "  %s, %s, %s, %s, %s\n",
		count(st.Pages, "page"),
		count(st.Tags, "tag"),
		count(st.Candidates, "candidate"),
		count(st.Lookups, "lookup"),
		spent.Round(time.Millisecond))
}

func count(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// reportTag writes the chosen tag to stderr. Not every registry supplies a
// date, so the digest is shown when there is none.
func reportTag(info TagInfo) {
	if info.LastUpdated != "" {
		stamp := info.LastUpdated
		if t, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
			stamp = t.Format("2006-01-02 15:04:05Z")
		}
		fmt.Fprintf(os.Stderr, ", Tag: %s,  Update: %s\n", info.Tag, stamp)
		return
	}

	if info.Digest != "" {
		fmt.Fprintf(os.Stderr, ", Tag: %s,  Digest: %s\n", info.Tag, info.Digest)
		return
	}

	fmt.Fprintf(os.Stderr, ", Tag: %s\n", info.Tag)
}
