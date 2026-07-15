package store

import "testing"

func TestSettings(t *testing.T) {
	s, _ := openTest(t, 10)

	// Unset key reads as empty, no error.
	v, err := s.Setting(ctx, "prefs")
	if err != nil {
		t.Fatal(err)
	}
	if v != "" {
		t.Fatalf("unset key = %q, want empty", v)
	}

	if err := s.SetSetting(ctx, "prefs", `{"theme":"dark"}`); err != nil {
		t.Fatal(err)
	}
	if v, _ = s.Setting(ctx, "prefs"); v != `{"theme":"dark"}` {
		t.Fatalf("got %q", v)
	}

	// Replace, not append.
	if err := s.SetSetting(ctx, "prefs", `{"theme":"light"}`); err != nil {
		t.Fatal(err)
	}
	if v, _ = s.Setting(ctx, "prefs"); v != `{"theme":"light"}` {
		t.Fatalf("after replace got %q", v)
	}

	// Keys are independent.
	if err := s.SetSetting(ctx, "other", "x"); err != nil {
		t.Fatal(err)
	}
	if v, _ = s.Setting(ctx, "prefs"); v != `{"theme":"light"}` {
		t.Fatalf("other key clobbered prefs: %q", v)
	}
}

func TestSettingsSurviveReopen(t *testing.T) {
	s, path := openTest(t, 10)
	if err := s.SetSetting(ctx, "prefs", `{"accent":"rose"}`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2 := reopen(t, path, 10)
	v, err := s2.Setting(ctx, "prefs")
	if err != nil {
		t.Fatal(err)
	}
	if v != `{"accent":"rose"}` {
		t.Fatalf("after reopen got %q", v)
	}
}
