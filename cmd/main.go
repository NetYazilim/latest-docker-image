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
	repo := ref.Repo
	cfg.Tag = ref.Filter
	// name, kullanıcıya gösterilen ve stdout'a yazılan tam ad (host dahil);
	// repo ise registry'ye sorulan yol.
	name := ref.Name()

	filter, err := regexp.Compile(cfg.Tag)
	if err != nil {
		return fmt.Errorf("invalid tag regex: %w", err)
	}

	reg := pickRegistry(ref)

	info, err := resolve(ctx, reg, repo, filter, cfg.Architecture, cfg.OS)
	if err != nil && !errors.Is(err, ErrNoMatch) {
		return fmt.Errorf("HTTP isteği başarısız: %w", err)
	}

	fmt.Fprintf(os.Stderr, "\nRepo.: %s, Arch.: %s, OS: %s, Filter: %s", name, cfg.Architecture, cfg.OS, cfg.Tag)

	// Hata durumunda stdout BOŞ kalmalı: README'deki
	// `docker pull $(ldi ...)` kalıbı aksi halde
	// "docker pull no-new-image!" komutunu çalıştırıyordu.
	// (os.Exit(-1) de gerçekte 255 kodunu üretiyordu.)
	if errors.Is(err, ErrNoMatch) {
		fmt.Fprintf(os.Stderr, ", Bulunamadı\n")
		os.Exit(1)
	}

	reportTag(info)
	fmt.Fprintf(os.Stdout, "%s:%s", name, info.Tag)
	return nil
}

// pickRegistry, referansa göre sürücüyü seçer.
//
// Docker Hub, Distribution protokolünü registry-1.docker.io üzerinden de
// konuşuyor; ama tescilli API'si tag + platform + tarihi tek çağrıda verdiği
// için oraya taşımak anlamsız olurdu: aynı bilgi tag başına iki ek istek
// ederdi ve manifest istekleri Hub'ın pull limitine sayılır.
func pickRegistry(ref Reference) Registry {
	if ref.Host != "" && !isDockerHub(ref.Host) {
		return newDistribution(ref.Host)
	}

	// Regex'in zorunlu literal parçası varsa Docker Hub'ın sunucu tarafı
	// `name=` filtresine verilir; sayfa sayısını düşürür. Distribution'da
	// böyle bir filtre yok.
	nameFilter := ""
	if rep, err := syntax.Parse(ref.Filter, syntax.Perl); err == nil {
		nameFilter = requiredLiteral(rep.Simplify())
	}
	return newDockerHub(nameFilter)
}

// isDockerHub, Docker Hub'ı adlandıran host takma adlarını tanır.
func isDockerHub(host string) bool {
	switch host {
	case "docker.io", "index.docker.io", "registry-1.docker.io", "registry.hub.docker.com":
		return true
	}
	return false
}

// reportTag, seçilen tag'i stderr'e yazar. Tarih her registry'de bulunmadığı
// için yoksa digest gösterilir.
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
