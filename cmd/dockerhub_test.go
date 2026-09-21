package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp/syntax"
	"strings"
	"testing"
)

func TestHubPagerFollowsNext(t *testing.T) {
	var srv *httptest.Server
	var paths []string
	page := 0

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		page++
		w.Header().Set("Content-Type", "application/json")
		if page == 1 {
			fmt.Fprintf(w, `{"next":%q,"results":[
				{"name":"1.0.0","content_type":"image","last_updated":"2024-01-02T03:04:05.678Z",
				 "images":[{"architecture":"amd64","os":"linux","status":"active"}]}]}`,
				srv.URL+"/sayfa2")
			return
		}
		fmt.Fprint(w, `{"next":"","results":[
			{"name":"2.0.0","content_type":"image",
			 "images":[{"architecture":"arm64","os":"linux","status":"active"},
			           {"architecture":"amd64","os":"linux","status":"inactive"}]}]}`)
	}))
	defer srv.Close()

	ctx := context.Background()
	hub := newDockerHub("")
	hub.baseURL = srv.URL

	pager := hub.Tags("traefik")

	first, err := pager.Next(ctx)
	if err != nil {
		t.Fatalf("birinci sayfa: %v", err)
	}
	if len(first) != 1 || first[0] != "1.0.0" {
		t.Fatalf("birinci sayfa = %v, beklenen [1.0.0]", first)
	}

	second, err := pager.Next(ctx)
	if err != nil {
		t.Fatalf("ikinci sayfa: %v", err)
	}
	if len(second) != 1 || second[0] != "2.0.0" {
		t.Fatalf("ikinci sayfa = %v, beklenen [2.0.0]", second)
	}

	if done, err := pager.Next(ctx); err != nil || done != nil {
		t.Fatalf("üçüncü çağrı = (%v, %v), beklenen (nil, nil)", done, err)
	}

	// Tek parçalı isim "library" namespace'ine alınmalı.
	if !strings.Contains(paths[0], "/v2/repositories/library/traefik/tags") {
		t.Errorf("istek yolu = %q, library/ öneki bekleniyordu", paths[0])
	}

	// Sayfalama sırasında görülen kayıtlar Inspect için saklanmalı: bu sürücüde
	// Inspect ağa çıkmaz.
	info, err := hub.Inspect(ctx, "traefik", "1.0.0")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if info.LastUpdated != "2024-01-02T03:04:05.678Z" {
		t.Errorf("LastUpdated = %q", info.LastUpdated)
	}
	if !platformMatches(info, "amd64", "linux") {
		t.Errorf("amd64/linux eşleşmeliydi: %+v", info.Platforms)
	}

	// status != active olan imaj platform listesine girmemeli.
	info, err = hub.Inspect(ctx, "traefik", "2.0.0")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if platformMatches(info, "amd64", "linux") {
		t.Errorf("inactive imaj eşleşmemeliydi: %+v", info.Platforms)
	}
	if !platformMatches(info, "arm64", "linux") {
		t.Errorf("arm64/linux eşleşmeliydi: %+v", info.Platforms)
	}
}

func TestHubInspectPluginIgnoresPlatform(t *testing.T) {
	hub := newDockerHub("")
	hub.seen["1.0.0"] = hubTag{Name: "1.0.0", ContentType: "plugin"}

	info, err := hub.Inspect(context.Background(), "x/y", "1.0.0")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !info.AnyPlatform {
		t.Error("plugin kaydı AnyPlatform olarak işaretlenmeliydi")
	}
	if !platformMatches(info, "s390x", "linux") {
		t.Error("plugin her platformu geçmeliydi")
	}
}

func TestHubFetchSurfacesAPIMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"object not found"}`)
	}))
	defer srv.Close()

	hub := newDockerHub("")
	hub.baseURL = srv.URL

	_, err := hub.Tags("x/y").Next(context.Background())
	if err == nil {
		t.Fatal("404 için hata bekleniyordu")
	}
	if !strings.Contains(err.Error(), "object not found") {
		t.Errorf("hata = %q, API mesajını taşımalıydı", err)
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("hata = %q, durum kodunu taşımalıydı", err)
	}
}

func TestApiMessage(t *testing.T) {
	tests := []struct {
		body string
		want string
	}{
		{`{"message":"rate limit"}`, "rate limit"},
		{`{"detail":"not found"}`, "not found"},
		{`{"message":"m","detail":"d"}`, "m"},
		{`{}`, ""},
		{`bozuk json`, ""},
	}

	for _, tc := range tests {
		if got := apiMessage([]byte(tc.body)); got != tc.want {
			t.Errorf("apiMessage(%s) = %q, beklenen %q", tc.body, got, tc.want)
		}
	}
}

func TestRequiredLiteral(t *testing.T) {
	tests := []struct {
		re   string
		want string
	}{
		{`(\d+)\.(\d+)\.(\d+)$`, ""},
		{`(\d+)\.(\d+)\.(\d+)-alpine$`, "-alpine"},
		{`-alpine$`, "-alpine"},
		{`v(\d+)-jammy`, "-jammy"},
		{`^(\d+)\.(\d+)\.(\d+)-alpine$`, "-alpine"},
		// Alternation: hiçbir dal zorunlu değil, literal kullanılmamalı.
		{`alpine|bookworm`, ""},
		{`(alpine|bookworm)$`, ""},
		// Opsiyonel grup zorunlu değil.
		{`(-alpine)?$`, ""},
		// Case-insensitive literal sunucu filtresiyle uyuşmayabilir.
		{`(?i)-ALPINE$`, ""},
		{``, ""},
		{`.*`, ""},
	}

	for _, tc := range tests {
		rep, err := syntax.Parse(tc.re, syntax.Perl)
		if err != nil {
			t.Fatalf("%q parse edilemedi: %v", tc.re, err)
		}
		if got := requiredLiteral(rep.Simplify()); got != tc.want {
			t.Errorf("requiredLiteral(%q) = %q, beklenen %q", tc.re, got, tc.want)
		}
	}
}
