package proxydial

import (
	"strings"
	"testing"
)

// A rejected proxy URL must not leak its credentials in the error message.
func TestParseRedactsCredentials(t *testing.T) {
	// Password chosen so it is unambiguous in the error text.
	_, err := Parse("socks5://alice:sup3rSecret@host:1080/nope") // path -> rejected
	if err == nil {
		t.Fatal("expected an error for a proxy URL with a path")
	}
	if strings.Contains(err.Error(), "sup3rSecret") || strings.Contains(err.Error(), "alice") {
		t.Fatalf("error leaked credentials: %v", err)
	}
	if !strings.Contains(err.Error(), "<redacted>@host:1080") {
		t.Fatalf("error should retain the redacted host: %v", err)
	}
}

func TestParse(t *testing.T) {
	ok := []string{
		"socks5://127.0.0.1:1080",
		"socks5h://tor:9050",
		"socks5://alice:pw@10.0.0.1:1080",
		"http://user:pass@proxy.example:3128",
	}
	for _, s := range ok {
		if _, err := Parse(s); err != nil {
			t.Errorf("Parse(%q) = %v, want ok", s, err)
		}
	}
	bad := []string{
		"socks5://noport",             // missing port
		"ftp://host:1080",             // wrong scheme
		"socks5://host:1080/path",     // unexpected path
		"://bad",                      // unparseable
		"",                            // empty
	}
	for _, s := range bad {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) = nil, want error", s)
		}
	}
}
