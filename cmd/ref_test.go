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
		// An empty tag means "every tag is a candidate".
		{in: "golang:", repo: "golang"},
		{in: `golang:-alpine$`, repo: "golang", filter: `-alpine$`},
		// A first component containing a dot is a registry address.
		{in: "registry.redhat.io/ubi9/ubi", host: "registry.redhat.io", repo: "ubi9/ubi"},
		{
			in:     `registry.redhat.io/ubi9/ubi:^9\.\d+$`,
			host:   "registry.redhat.io",
			repo:   "ubi9/ubi",
			filter: `^9\.\d+$`,
		},
		{in: "quay.io/prometheus/node-exporter", host: "quay.io", repo: "prometheus/node-exporter"},
		// A host:port must not be mistaken for the tag separator.
		{in: `localhost:5000/myimg:1\.0$`, host: "localhost:5000", repo: "myimg", filter: `1\.0$`},
		{in: "localhost/myimg", host: "localhost", repo: "myimg"},
		{in: "ubuntu@sha256:abc", repo: "ubuntu", digest: "sha256:abc"},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseReference(tc.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Host != tc.host {
				t.Errorf("Host = %q, want %q", got.Host, tc.host)
			}
			if got.Repo != tc.repo {
				t.Errorf("Repo = %q, want %q", got.Repo, tc.repo)
			}
			if got.Filter != tc.filter {
				t.Errorf("Filter = %q, want %q", got.Filter, tc.filter)
			}
			if got.Digest != tc.digest {
				t.Errorf("Digest = %q, want %q", got.Digest, tc.digest)
			}
		})
	}
}

func TestReferenceName(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		// Without a host the output is unchanged.
		{"traefik", "traefik"},
		{"grafana/grafana-oss", "grafana/grafana-oss"},
		// With a host the output must carry it, or docker pull looks for the
		// wrong image.
		{"registry.redhat.io/ubi9/ubi", "registry.redhat.io/ubi9/ubi"},
		{`quay.io/prometheus/node-exporter:^v(\d+)\.(\d+)\.(\d+)$`, "quay.io/prometheus/node-exporter"},
		{"localhost:5000/myimg", "localhost:5000/myimg"},
	}

	for _, tc := range tests {
		ref, err := ParseReference(tc.in)
		if err != nil {
			t.Fatalf("%q: unexpected error: %v", tc.in, err)
		}
		if got := ref.Name(); got != tc.want {
			t.Errorf("ParseReference(%q).Name() = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseReferenceErrors(t *testing.T) {
	// Every one of these used to build a wrong URL silently and degrade into
	// "not found".
	bad := []string{"", "   ", "repo@", "@sha256:abc", "repo:a:b"}

	for _, in := range bad {
		if ref, err := ParseReference(in); err == nil {
			t.Errorf("ParseReference(%q) should have returned an error, got: %+v", in, ref)
		}
	}
}
