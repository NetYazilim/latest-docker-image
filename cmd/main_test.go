package main

import (
	"regexp"
	"testing"
)

func TestFilterTags(t *testing.T) {
	tags := []Tag{
		{
			Name:        "1.0.0",
			ContentType: "image",
			Images: []TagDetail{
				{Architecture: "amd64", OS: "linux", Status: "active"},
			},
		},
		{
			Name:        "1.1.0-beta",
			ContentType: "image",
			Images: []TagDetail{
				{Architecture: "amd64", OS: "linux", Status: "active"},
			},
		},
		{
			Name:        "2.0.0",
			ContentType: "image",
			Images: []TagDetail{
				{Architecture: "arm64", OS: "linux", Status: "active"},
			},
		},
	}

	re := regexp.MustCompile(`.*`)

	// Test linux/amd64
	results := filterTags(tags, re, "amd64", "linux")
	if len(results) != 1 {
		t.Errorf("Expected 1 result, got %d", len(results))
	}
	if results[0].Tag != "1.0.0" {
		t.Errorf("Expected Tag 1.0.0, got %s", results[0].Tag)
	}

	// Test linux/arm64
	results = filterTags(tags, re, "arm64", "linux")
	if len(results) != 1 {
		t.Errorf("Expected 1 result, got %d", len(results))
	}
	if results[0].Tag != "2.0.0" {
		t.Errorf("Expected Tag 2.0.0, got %s", results[0].Tag)
	}
}

func TestSortResults(t *testing.T) {
	results := []Result{
		{Tag: "1.0.0"},
		{Tag: "1.1.0"},
		{Tag: "1.1.0-security-01"},
		{Tag: "0.9.0"},
	}

	sortResults(results)

	if results[0].Tag != "1.1.0-security-01" {
		t.Errorf("Expected 1.1.0-security-01 at index 0, got %s", results[0].Tag)
	}
	if results[1].Tag != "1.1.0" {
		t.Errorf("Expected 1.1.0 at index 1, got %s", results[1].Tag)
	}
	if results[2].Tag != "1.0.0" {
		t.Errorf("Expected 1.0.0 at index 2, got %s", results[2].Tag)
	}
}
