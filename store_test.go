package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestStoreSaveLoadRoundTrip(t *testing.T) {
	s := &store{dir: filepath.Join(t.TempDir(), "tenants")}

	// Deliberately out of sorted order — loadAll is supposed to normalize it.
	want := []tenantState{
		{OrgID: "bbb", OrgName: "second", AppUserID: "u2", AccessToken: "a2", RefreshToken: "r2", Expires: 2},
		{OrgID: "aaa", OrgName: "first", AppUserID: "u1", AccessToken: "a1", RefreshToken: "r1", Expires: 1},
	}
	for _, ts := range want {
		if err := s.save(ts); err != nil {
			t.Fatalf("save(%s): %v", ts.OrgID, err)
		}
	}

	got, bad, err := s.loadAll()
	if err != nil || len(bad) != 0 {
		t.Fatalf("loadAll: %v, bad=%v", err, bad)
	}
	if len(got) != 2 {
		t.Fatalf("loadAll() = %+v, want 2 workspaces", got)
	}
	if got[0].OrgID != "aaa" || got[1].OrgID != "bbb" {
		t.Fatalf("loadAll() = %+v, want ordering by organization id", got)
	}
	if got[0].AccessToken != "a1" || got[0].RefreshToken != "r1" || got[0].Expires != 1 || got[0].AppUserID != "u1" {
		t.Fatalf("loadAll()[0] = %+v, want the material save was given", got[0])
	}
}

func TestStoreSaveIsPrivateAndOverwrites(t *testing.T) {
	s := &store{dir: filepath.Join(t.TempDir(), "tenants")}
	if err := s.save(tenantState{OrgID: "org", AccessToken: "first", RefreshToken: "r"}); err != nil {
		t.Fatalf("save: %v", err)
	}

	info, err := os.Stat(s.path("org"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("tenant file mode = %o, want 600 — it holds a refresh token", perm)
	}

	// A rotation rewrites in place rather than accumulating files.
	if err := s.save(tenantState{OrgID: "org", AccessToken: "second", RefreshToken: "r"}); err != nil {
		t.Fatalf("save (rotation): %v", err)
	}
	got, bad, err := s.loadAll()
	if err != nil || len(bad) != 0 {
		t.Fatalf("loadAll: %v, bad=%v", err, bad)
	}
	if len(got) != 1 || got[0].AccessToken != "second" {
		t.Fatalf("loadAll() = %+v, want one workspace holding the rotated token", got)
	}
}

func TestStoreLoadAllMissingDirIsEmpty(t *testing.T) {
	s := &store{dir: filepath.Join(t.TempDir(), "never-created")}
	got, bad, err := s.loadAll()
	if err != nil || len(bad) != 0 {
		t.Fatalf("loadAll on a missing dir = %v, bad=%v, want nothing (not-yet-installed is not a failure)", err, bad)
	}
	if len(got) != 0 {
		t.Fatalf("loadAll() = %+v, want empty", got)
	}
}

// A corrupt tenant file must cost only that workspace. loadTenants runs under
// Restart=always, so failing the whole load would crash-loop the process and
// take the install endpoint — the only way to repair the store — down with it.
func TestStoreLoadAllSurvivesOneBadFile(t *testing.T) {
	s := &store{dir: filepath.Join(t.TempDir(), "tenants")}
	if err := s.save(tenantState{OrgID: "good", AccessToken: "a"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	for name, body := range map[string]string{
		"truncated.json": "",
		"garbage.json":   "{not json",
		"anonymous.json": `{"access_token":"a"}`, // no organization_id to key it by
	} {
		if err := os.WriteFile(filepath.Join(s.dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	got, bad, err := s.loadAll()
	if err != nil {
		t.Fatalf("loadAll = %v, want the readable installs plus a report of the rest", err)
	}
	if len(bad) != 3 {
		t.Fatalf("loadAll reported %d bad files, want 3", len(bad))
	}
	if len(got) != 1 || got[0].OrgID != "good" {
		t.Fatalf("loadAll() = %+v, want the one readable workspace", got)
	}
}

// Two writers for the same workspace — a completing install and a token
// refresh — must not interleave into a torn file or leave temp files behind.
func TestStoreSaveIsSerialized(t *testing.T) {
	s := &store{dir: filepath.Join(t.TempDir(), "tenants")}
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.save(tenantState{OrgID: "org", AccessToken: strings.Repeat("t", i+1)}); err != nil {
				t.Errorf("save: %v", err)
			}
		}()
	}
	wg.Wait()

	got, bad, err := s.loadAll()
	if err != nil || len(bad) != 0 {
		t.Fatalf("loadAll after concurrent saves = %v, bad=%v — a torn file would fail to decode", err, bad)
	}
	if len(got) != 1 {
		t.Fatalf("loadAll() = %+v, want exactly one workspace", got)
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("store holds %d files, want 1 — temp files must not survive", len(entries))
	}
}

func TestStoreRejectsUnsafeKeysAndUnsetDir(t *testing.T) {
	s := &store{dir: t.TempDir()}
	for _, org := range []string{"", ".", "..", "../escape", "has/slash", "has space"} {
		if err := s.save(tenantState{OrgID: org, AccessToken: "a"}); err == nil {
			t.Fatalf("save(%q) = nil error, want a refusal", org)
		}
	}
	if safeKey("f9a4dcde-1f1d-43e1-a9c6-dbded1d624b4") != true {
		t.Fatal("safeKey should accept a plain workspace UUID")
	}

	unset := &store{}
	if err := unset.save(tenantState{OrgID: "org", AccessToken: "a"}); err == nil {
		t.Fatal("save with no state dir = nil error, want a refusal rather than a silently dropped credential")
	}
}
