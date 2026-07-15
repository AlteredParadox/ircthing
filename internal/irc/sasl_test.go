package irc

import (
	"bytes"
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
)

func TestSASLPlain(t *testing.T) {
	cases := []struct {
		name                     string
		authzid, authcid, passwd string
		want                     []byte
	}{
		{
			// RFC 4616 §4 example: "\0tim\0tanstaaftanstaaf"
			name:    "rfc4616 example",
			authcid: "tim",
			passwd:  "tanstaaftanstaaf",
			want:    []byte("\x00tim\x00tanstaaftanstaaf"),
		},
		{
			name:    "with authzid",
			authzid: "admin",
			authcid: "user",
			passwd:  "pw",
			want:    []byte("admin\x00user\x00pw"),
		},
		{
			name: "all empty still has two separators",
			want: []byte("\x00\x00"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := saslPlain(tc.authzid, tc.authcid, tc.passwd); !bytes.Equal(got, tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSASLPlainBase64RFC4616(t *testing.T) {
	// The RFC 4616 example encodes to this exact base64 string.
	got := base64.StdEncoding.EncodeToString(saslPlain("", "tim", "tanstaaftanstaaf"))
	if want := "AHRpbQB0YW5zdGFhZnRhbnN0YWFm"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestChunkAuthenticate(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "empty response is a lone plus",
			in:   "",
			want: []string{"+"},
		},
		{
			name: "short single chunk",
			in:   "YWJj",
			want: []string{"YWJj"},
		},
		{
			name: "399 bytes stays one chunk",
			in:   strings.Repeat("a", 399),
			want: []string{strings.Repeat("a", 399)},
		},
		{
			name: "exactly 400 needs a trailing plus",
			in:   strings.Repeat("a", 400),
			want: []string{strings.Repeat("a", 400), "+"},
		},
		{
			name: "401 splits without trailing plus",
			in:   strings.Repeat("a", 401),
			want: []string{strings.Repeat("a", 400), "a"},
		},
		{
			name: "800 splits into two full chunks plus terminator",
			in:   strings.Repeat("a", 800),
			want: []string{strings.Repeat("a", 400), strings.Repeat("a", 400), "+"},
		},
		{
			name: "1000 splits into 400/400/200",
			in:   strings.Repeat("a", 1000),
			want: []string{strings.Repeat("a", 400), strings.Repeat("a", 400), strings.Repeat("a", 200)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := chunkAuthenticate(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("chunk lengths: got %v, want %v", lens(got), lens(tc.want))
			}
		})
	}
}

func lens(ss []string) []int {
	out := make([]int, len(ss))
	for i, s := range ss {
		out[i] = len(s)
	}
	return out
}
