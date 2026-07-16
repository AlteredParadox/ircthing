package store

import (
	"context"
	"testing"
	"time"
)

func TestNetworkConfigCRUD(t *testing.T) {
	s, _ := openTest(t, 10)
	defer s.Close()
	ctx := context.Background()

	// Seed only fills an empty table.
	seeded, err := s.SeedNetworkConfigs(ctx, []NetworkConfig{{Name: "libera", Config: `{"addr":"a:1"}`}})
	if err != nil || !seeded {
		t.Fatalf("seed = %v, %v; want true, nil", seeded, err)
	}
	seeded, err = s.SeedNetworkConfigs(ctx, []NetworkConfig{{Name: "other", Config: `{}`}})
	if err != nil || seeded {
		t.Fatalf("second seed = %v, %v; want false, nil", seeded, err)
	}

	if err := s.PutNetworkConfig(ctx, "oftc", `{"addr":"b:2"}`); err != nil {
		t.Fatal(err)
	}
	if err := s.PutNetworkConfig(ctx, "oftc", `{"addr":"b:3"}`); err != nil {
		t.Fatal(err) // upsert
	}
	configs, err := s.NetworkConfigs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(configs) != 2 || configs[0].Name != "libera" || configs[1].Config != `{"addr":"b:3"}` {
		t.Fatalf("configs = %+v", configs)
	}

	if err := s.DeleteNetworkConfig(ctx, "libera"); err != nil {
		t.Fatal(err)
	}
	configs, _ = s.NetworkConfigs(ctx)
	if len(configs) != 1 {
		t.Fatalf("after delete: %+v", configs)
	}
}

func TestDeleteAndRenameNetworkData(t *testing.T) {
	s, _ := openTest(t, 10)
	defer s.Close()
	ctx := context.Background()

	msg := Message{Time: time.Now(), Sender: "a", Command: "PRIVMSG", Raw: ":a PRIVMSG #x :hi"}
	if _, err := s.Append(ctx, "netA", "#x", msg); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(ctx, "netB", "#y", msg); err != nil {
		t.Fatal(err)
	}

	// Rename carries history to the new name.
	if err := s.RenameNetworkData(ctx, "netA", "netC"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Latest(ctx, "netC", "#x", 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("after rename: %v, %v", got, err)
	}
	// Renaming onto a name with existing history is refused.
	if err := s.RenameNetworkData(ctx, "netC", "netB"); err == nil {
		t.Fatal("rename onto existing data: want error")
	}

	// Delete removes all rows for the network; other networks keep theirs.
	if err := s.DeleteNetworkData(ctx, "netC"); err != nil {
		t.Fatal(err)
	}
	bufs, err := s.Buffers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(bufs) != 1 || bufs[0].Network != "netB" {
		t.Fatalf("buffers after delete = %+v", bufs)
	}
	// The deleted network's caches are gone: a fresh append recreates
	// cleanly rather than writing into a stale ring.
	if _, err := s.Append(ctx, "netC", "#x", msg); err != nil {
		t.Fatal(err)
	}
	got, err = s.Latest(ctx, "netC", "#x", 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("after re-append: %v, %v", got, err)
	}
}

func TestAppendExistingAfterDeleteBuffer(t *testing.T) {
	s, _ := openTest(t, 10)
	defer s.Close()
	ctx := context.Background()

	msg := Message{Time: time.Now(), Sender: "AlteredParadox", Command: "PART", Raw: ":AlteredParadox PART #x"}
	if _, err := s.Append(ctx, "net", "#x", msg); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteBuffer(ctx, "net", "#x"); err != nil {
		t.Fatal(err)
	}
	// The PART echo arriving after close must not resurrect the buffer.
	got, err := s.AppendExisting(ctx, "net", "#x", msg)
	if err != nil || got.ID != 0 {
		t.Fatalf("AppendExisting after delete = %+v, %v; want dropped", got, err)
	}
	bufs, err := s.Buffers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(bufs) != 0 {
		t.Fatalf("buffer resurrected: %+v", bufs)
	}
	// Into an existing buffer it still lands normally.
	if _, err := s.Append(ctx, "net", "#y", msg); err != nil {
		t.Fatal(err)
	}
	if got, err := s.AppendExisting(ctx, "net", "#y", msg); err != nil || got.ID == 0 {
		t.Fatalf("AppendExisting into live buffer = %+v, %v", got, err)
	}
}
