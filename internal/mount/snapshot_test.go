package mount

import "testing"

func TestNewSnapshotNormalizesModesAndDigest(t *testing.T) {
	s, err := NewSnapshot("builtin/using-ninea", "using-ninea", "v1", 3, []File{{Path: "SKILL.md", Mode: 0o666, Data: []byte("hello")}, {Path: "scripts/invoke", Mode: 0o777, Data: []byte("#!/bin/sh\n")}})
	if err != nil {
		t.Fatal(err)
	}
	if s.Files[0].Mode != 0o444 || s.Files[1].Mode != 0o555 {
		t.Fatalf("modes=%o,%o", s.Files[0].Mode, s.Files[1].Mode)
	}
	if s.Digest == "" {
		t.Fatal("empty digest")
	}
	again, _ := NewSnapshot(s.LogicalID, s.Name, s.Version, s.CatalogRevision, []File{{Path: "scripts/invoke", Mode: 0o700, Data: []byte("#!/bin/sh\n")}, {Path: "SKILL.md", Mode: 0o600, Data: []byte("hello")}})
	if s.Digest != again.Digest {
		t.Fatalf("digest not deterministic: %s != %s", s.Digest, again.Digest)
	}
}

func TestNewSnapshotRejectsUnsafeAndDuplicatePaths(t *testing.T) {
	for _, files := range [][]File{{{Path: "../escape", Data: []byte("x")}}, {{Path: "x", Data: []byte("a")}, {Path: "x", Data: []byte("b")}}} {
		if _, err := NewSnapshot("id", "name", "v1", 1, files); err == nil {
			t.Fatalf("accepted %#v", files)
		}
	}
	if _, err := NewSnapshot("id", "../escape", "v1", 1, nil); err == nil {
		t.Fatal("unsafe name accepted")
	}
}
