package main

import (
	"errors"
	"fmt"
	"strings"
)

// Reference is an image reference as given on the command line:
//
//	[host[:port]/]path[:tag-regex][@digest]
//
// Parsing is driver independent; namespace normalisation (such as Docker Hub's
// "library/" prefix) is the relevant driver's job.
type Reference struct {
	// Host is the registry address. Empty means Docker Hub.
	Host string
	// Repo is the repository path as the registry expects it.
	Repo string
	// Filter is the regex used to select a tag. Empty means every tag is a
	// candidate.
	Filter string
	// Digest is the pinned reference given with @sha256:...
	Digest string
}

// Name returns the pullable full name of the reference: "host/path" when a
// host was given, the path itself otherwise. This is the name ldi must write to
// stdout; without it `docker pull $(ldi registry.redhat.io/ubi9/ubi)` would drop
// the host and look for the wrong image.
func (r Reference) Name() string {
	if r.Host == "" {
		return r.Repo
	}
	return r.Host + "/" + r.Repo
}

// ParseReference splits a reference into its parts.
//
// The tag separator is the last colon after the last slash, so a host:port
// cannot be mistaken for a tag regex. The first component counts as a registry
// address when it contains a dot or a colon, or is "localhost" - Docker's own
// rule.
func ParseReference(s string) (Reference, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Reference{}, errors.New("empty image name")
	}

	var ref Reference

	if i := strings.LastIndex(s, "@"); i != -1 {
		ref.Digest = s[i+1:]
		s = s[:i]
		if ref.Digest == "" {
			return Reference{}, errors.New("no digest after @")
		}
		if s == "" {
			return Reference{}, errors.New("no repository name before the digest")
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
		return Reference{}, errors.New("no repository name")
	}
	if strings.ContainsAny(s, ":@") {
		return Reference{}, fmt.Errorf("invalid repository name %q", s)
	}

	ref.Repo = s
	return ref, nil
}
