package api

import "testing"

func TestExtractMeta(t *testing.T) {
	cases := []struct {
		name                           string
		doc                            string
		wantTitle, wantDesc, wantImage string
		wantSite                       string
	}{
		{
			name: "opengraph tags",
			doc: `<html><head>
				<meta property="og:title" content="Cool Page">
				<meta property="og:description" content="A very cool page.">
				<meta property="og:image" content="https://ex.com/img.png">
				<meta property="og:site_name" content="Example">
				<title>fallback title</title>
			</head><body>ignored</body></html>`,
			wantTitle: "Cool Page", wantDesc: "A very cool page.",
			wantImage: "https://ex.com/img.png", wantSite: "Example",
		},
		{
			name:      "title element fallback",
			doc:       `<head><title>Just a Title</title></head>`,
			wantTitle: "Just a Title",
		},
		{
			name: "twitter card when no opengraph",
			doc: `<head>
				<meta name="twitter:title" content="Tweet Title">
				<meta name="twitter:description" content="tw desc">
				<meta name="twitter:image" content="https://ex.com/t.jpg">
			</head>`,
			wantTitle: "Tweet Title", wantDesc: "tw desc", wantImage: "https://ex.com/t.jpg",
		},
		{
			name: "attribute order and single quotes",
			doc: `<head><meta content='Ordered' property='og:title'>
				<meta name=description content=bare-desc></head>`,
			wantTitle: "Ordered", wantDesc: "bare-desc",
		},
		{
			name:      "html entities are unescaped",
			doc:       `<head><meta property="og:title" content="Tom &amp; Jerry &lt;3"></head>`,
			wantTitle: "Tom & Jerry <3",
		},
		{
			name:      "relative image resolved against page url",
			doc:       `<head><meta property="og:image" content="/assets/pic.png"></head>`,
			wantImage: "https://site.example/assets/pic.png",
		},
		{
			name:      "whitespace collapsed in title",
			doc:       "<head><title>  spread   over\n  lines  </title></head>",
			wantTitle: "spread over lines",
		},
		{
			name:      "body content is ignored",
			doc:       `<head><title>Head</title></head><body><meta property="og:title" content="Body"><h1>x</h1></body>`,
			wantTitle: "Head",
		},
		{
			name: "opengraph wins over twitter and title",
			doc: `<head>
				<title>plain</title>
				<meta name="twitter:title" content="tw">
				<meta property="og:title" content="og">
			</head>`,
			wantTitle: "og",
		},
		{
			name:      "no metadata yields empties",
			doc:       `<head></head><body>nothing</body>`,
			wantTitle: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pv := PreviewData{URL: "https://site.example/page"}
			extractMeta(tc.doc, "https://site.example/page", &pv)
			if pv.Title != tc.wantTitle {
				t.Errorf("title = %q, want %q", pv.Title, tc.wantTitle)
			}
			if pv.Description != tc.wantDesc {
				t.Errorf("desc = %q, want %q", pv.Description, tc.wantDesc)
			}
			if pv.Image != tc.wantImage {
				t.Errorf("image = %q, want %q", pv.Image, tc.wantImage)
			}
			if pv.SiteName != tc.wantSite {
				t.Errorf("site = %q, want %q", pv.SiteName, tc.wantSite)
			}
		})
	}
}

func TestClip(t *testing.T) {
	if got := clip("short", 100); got != "short" {
		t.Errorf("got %q", got)
	}
	if got := clip("abcdefghij", 5); got != "abcde…" {
		t.Errorf("got %q", got)
	}
	if got := clip("a\n\t b   c", 100); got != "a b c" {
		t.Errorf("whitespace not collapsed: %q", got)
	}
}

func TestResolveURL(t *testing.T) {
	cases := []struct{ page, ref, want string }{
		{"https://x.com/a/b", "/img.png", "https://x.com/img.png"},
		{"https://x.com/a/b", "pic.jpg", "https://x.com/a/pic.jpg"},
		{"https://x.com/", "https://cdn.x.com/i.png", "https://cdn.x.com/i.png"},
		{"https://x.com/", "//cdn.x.com/i.png", "https://cdn.x.com/i.png"},
		{"https://x.com/", "javascript:alert(1)", ""}, // non-http rejected
		{"https://x.com/", "ftp://x.com/i", ""},
	}
	for _, tc := range cases {
		if got := resolveURL(tc.page, tc.ref); got != tc.want {
			t.Errorf("resolveURL(%q, %q) = %q, want %q", tc.page, tc.ref, got, tc.want)
		}
	}
}

func TestIsImageType(t *testing.T) {
	yes := []string{"image/png", "image/jpeg", "IMAGE/JPEG", "image/gif; charset=binary", "image/webp"}
	no := []string{"text/html", "application/json", "", "image/svg+xml"}
	for _, ct := range yes {
		if !isImageType(ct) {
			t.Errorf("isImageType(%q) = false", ct)
		}
	}
	for _, ct := range no {
		if isImageType(ct) {
			t.Errorf("isImageType(%q) = true", ct)
		}
	}
}
