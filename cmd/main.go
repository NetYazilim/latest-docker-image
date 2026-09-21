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

// Config, komut satırından gelen ayarlar.
type Config struct {
	Architecture string
	OS           string
	Tag          string
}

var (
	cfg     Config
	Version = "1.6.0"
)

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
		return fmt.Errorf("@digest ile sabit referans desteklenmiyor: %s", ref.Digest)
	}
	if ref.Host != "" {
		// Faz 1'de OCI Distribution sürücüsü burayı dolduracak. O zamana kadar
		// sessizce Docker Hub'a sormak yerine açıkça söylüyoruz.
		return fmt.Errorf("%s: bu registry için sürücü yok, şimdilik yalnız Docker Hub destekleniyor", ref.Host)
	}

	repo := ref.Repo
	cfg.Tag = ref.Filter

	filter, err := regexp.Compile(cfg.Tag)
	if err != nil {
		return fmt.Errorf("invalid tag regex: %w", err)
	}

	// Regex'in zorunlu literal parçası varsa Docker Hub'ın sunucu tarafı
	// `name=` filtresine verilir; sayfa sayısını düşürür.
	nameFilter := ""
	if rep, perr := syntax.Parse(cfg.Tag, syntax.Perl); perr == nil {
		nameFilter = requiredLiteral(rep.Simplify())
	}

	var reg Registry = newDockerHub(nameFilter)

	info, err := resolve(ctx, reg, repo, filter, cfg.Architecture, cfg.OS)
	if err != nil && !errors.Is(err, ErrNoMatch) {
		return fmt.Errorf("HTTP isteği başarısız: %w", err)
	}

	fmt.Fprintf(os.Stderr, "\nRepo.: %s, Arch.: %s, OS: %s, Filter: %s", repo, cfg.Architecture, cfg.OS, cfg.Tag)

	// Hata durumunda stdout BOŞ kalmalı: README'deki
	// `docker pull $(ldi ...)` kalıbı aksi halde
	// "docker pull no-new-image!" komutunu çalıştırıyordu.
	// (os.Exit(-1) de gerçekte 255 kodunu üretiyordu.)
	if errors.Is(err, ErrNoMatch) {
		fmt.Fprintf(os.Stderr, ", Bulunamadı\n")
		os.Exit(1)
	}

	lastUpdated := info.LastUpdated
	if t, perr := time.Parse(time.RFC3339Nano, lastUpdated); perr == nil {
		lastUpdated = t.Format("2006-01-02 15:04:05Z")
	}

	fmt.Fprintf(os.Stderr, ", Tag: %s,  Update: %s\n", info.Tag, lastUpdated)
	fmt.Fprintf(os.Stdout, "%s:%s", repo, info.Tag)
	return nil
}
