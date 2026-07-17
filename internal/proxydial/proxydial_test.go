package proxydial

import "testing"

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
