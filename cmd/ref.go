package main

import (
	"errors"
	"fmt"
	"strings"
)

// Reference, komut satırında verilen imaj referansı:
//
//	[host[:port]/]yol[:tag-regex][@digest]
//
// Ayrıştırma sürücüden bağımsızdır; namespace normalizasyonu (Docker Hub'ın
// "library/" öneki gibi) ilgili sürücünün işi.
type Reference struct {
	// Host, registry adresi. Boşsa Docker Hub kastedilmiştir.
	Host string
	// Repo, registry'nin beklediği repo yolu.
	Repo string
	// Filter, tag seçimi için regex. Boşsa tüm tag'ler aday.
	Filter string
	// Digest, @sha256:... ile verilen sabit referans.
	Digest string
}

// Name, referansın çekilebilir tam adını döndürür: host verildiyse
// "host/yol", verilmediyse yolun kendisi. ldi'nin stdout'a yazdığı ad bu
// olmalı, yoksa `docker pull $(ldi registry.redhat.io/ubi9/ubi)` host'u
// kaybedip yanlış imajı arar.
func (r Reference) Name() string {
	if r.Host == "" {
		return r.Repo
	}
	return r.Host + "/" + r.Repo
}

// ParseReference, referansı parçalarına ayırır.
//
// Tag ayırıcısı "son / işaretinden sonraki son :" olarak bulunur; böylece
// host:port ile tag regex'i karışmaz. İlk bileşen nokta ya da iki nokta
// içeriyorsa veya "localhost" ise registry adresi sayılır - Docker'ın kendi
// kuralı da budur.
func ParseReference(s string) (Reference, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Reference{}, errors.New("imaj adı boş")
	}

	var ref Reference

	if i := strings.LastIndex(s, "@"); i != -1 {
		ref.Digest = s[i+1:]
		s = s[:i]
		if ref.Digest == "" {
			return Reference{}, errors.New("@ işaretinden sonra digest yok")
		}
		if s == "" {
			return Reference{}, errors.New("digest öncesinde repo adı yok")
		}
	}

	if colon := strings.LastIndex(s, ":"); colon > strings.LastIndex(s, "/") {
		ref.Filter = s[colon+1:]
		s = s[:colon]
	}

	if i := strings.Index(s, "/"); i != -1 {
		if first := s[:i]; first == "localhost" || strings.ContainsAny(first, ".:") {
			ref.Host = first
			s = s[i+1:]
		}
	}

	if s == "" {
		return Reference{}, errors.New("repo adı yok")
	}
	if strings.ContainsAny(s, ":@") {
		return Reference{}, fmt.Errorf("geçersiz repo adı %q", s)
	}

	ref.Repo = s
	return ref, nil
}
