package main

import "testing"

func TestParseReference(t *testing.T) {
	tests := []struct {
		in     string
		host   string
		repo   string
		filter string
		digest string
	}{
		{in: "traefik", repo: "traefik"},
		{in: "grafana/grafana-oss", repo: "grafana/grafana-oss"},
		{
			in:     `grafana/grafana-oss:(\d+)\.(\d+)\.(\d+)$`,
			repo:   "grafana/grafana-oss",
			filter: `(\d+)\.(\d+)\.(\d+)$`,
		},
		// Boş tag, "tüm tag'ler aday" demek.
		{in: "golang:", repo: "golang"},
		{in: `golang:-alpine$`, repo: "golang", filter: `-alpine$`},
		// Nokta içeren ilk bileşen registry adresidir.
		{in: "registry.redhat.io/ubi9/ubi", host: "registry.redhat.io", repo: "ubi9/ubi"},
		{
			in:     `registry.redhat.io/ubi9/ubi:^9\.\d+$`,
			host:   "registry.redhat.io",
			repo:   "ubi9/ubi",
			filter: `^9\.\d+$`,
		},
		{in: "quay.io/prometheus/node-exporter", host: "quay.io", repo: "prometheus/node-exporter"},
		// host:port ile tag ayırıcısı karışmamalı.
		{in: `localhost:5000/myimg:1\.0$`, host: "localhost:5000", repo: "myimg", filter: `1\.0$`},
		{in: "localhost/myimg", host: "localhost", repo: "myimg"},
		{in: "ubuntu@sha256:abc", repo: "ubuntu", digest: "sha256:abc"},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseReference(tc.in)
			if err != nil {
				t.Fatalf("beklenmeyen hata: %v", err)
			}
			if got.Host != tc.host {
				t.Errorf("Host = %q, beklenen %q", got.Host, tc.host)
			}
			if got.Repo != tc.repo {
				t.Errorf("Repo = %q, beklenen %q", got.Repo, tc.repo)
			}
			if got.Filter != tc.filter {
				t.Errorf("Filter = %q, beklenen %q", got.Filter, tc.filter)
			}
			if got.Digest != tc.digest {
				t.Errorf("Digest = %q, beklenen %q", got.Digest, tc.digest)
			}
		})
	}
}

func TestReferenceName(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		// Host'suz referansta çıktı aynen korunur.
		{"traefik", "traefik"},
		{"grafana/grafana-oss", "grafana/grafana-oss"},
		// Host verildiyse çıktı host'u taşımalı, yoksa docker pull yanlış
		// imajı arar.
		{"registry.redhat.io/ubi9/ubi", "registry.redhat.io/ubi9/ubi"},
		{`quay.io/prometheus/node-exporter:^v(\d+)\.(\d+)\.(\d+)$`, "quay.io/prometheus/node-exporter"},
		{"localhost:5000/myimg", "localhost:5000/myimg"},
	}

	for _, tc := range tests {
		ref, err := ParseReference(tc.in)
		if err != nil {
			t.Fatalf("%q: beklenmeyen hata: %v", tc.in, err)
		}
		if got := ref.Name(); got != tc.want {
			t.Errorf("ParseReference(%q).Name() = %q, beklenen %q", tc.in, got, tc.want)
		}
	}
}

func TestParseReferenceErrors(t *testing.T) {
	// Bunların hepsi eskiden sessizce yanlış bir URL üretip "Bulunamadı"
	// sonucuna düşüyordu.
	bad := []string{"", "   ", "repo@", "@sha256:abc", "repo:a:b"}

	for _, in := range bad {
		if ref, err := ParseReference(in); err == nil {
			t.Errorf("ParseReference(%q) hata döndürmeliydi, dönen: %+v", in, ref)
		}
	}
}
