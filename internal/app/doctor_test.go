package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/9a/internal/catalog"
	"github.com/gopact-ai/9a/internal/store"
)

func TestDoctorIsReadOnlyByDefaultAndRepairsGatewayOnRequest(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "ninea.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	a := New(db)
	if err := a.Bootstrap(ctx, "secret"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cleanupReadOnlyProjection(t, root)

	report, err := a.Doctor(ctx, "admin", root, false)
	if err != nil || report.Healthy || report.Fixed != 0 {
		t.Fatalf("read-only report=%#v err=%v", report, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".agents")); !os.IsNotExist(err) {
		t.Fatalf("read-only doctor changed workspace: %v", err)
	}

	report, err = a.Doctor(ctx, "admin", root, true)
	if err != nil || !report.Healthy || report.Fixed != 1 {
		t.Fatalf("fixed report=%#v err=%v", report, err)
	}
	skill := filepath.Join(root, ".agents", "skills", "using-ninea", "SKILL.md")
	if _, err := os.Stat(skill); err != nil {
		t.Fatalf("gateway skill: %v", err)
	}

	target := filepath.Dir(skill)
	if err := os.Chmod(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(skill, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skill, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err = a.Doctor(ctx, "admin", root, false)
	if err != nil || report.Healthy || report.Fixed != 0 {
		t.Fatalf("tampered report=%#v err=%v", report, err)
	}
	data, err := os.ReadFile(skill)
	if err != nil || string(data) != "tampered" {
		t.Fatalf("read-only doctor repaired content=%q err=%v", data, err)
	}
	report, err = a.Doctor(ctx, "admin", root, true)
	if err != nil || !report.Healthy || report.Fixed != 1 {
		t.Fatalf("repair report=%#v err=%v", report, err)
	}
	data, err = os.ReadFile(skill)
	if err != nil || string(data) == "tampered" {
		t.Fatalf("gateway was not repaired: %q err=%v", data, err)
	}
}

func TestDoctorDetectsAndRebuildsStaleIntegrationRuntime(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"city": r.URL.Query().Get("city"), "temperature": 23})
	}))
	defer server.Close()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "ninea.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	a := New(db)
	if err := a.Bootstrap(ctx, "secret"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cleanupReadOnlyProjection(t, root)
	source := []byte(strings.Replace(appDeclarativeSource, "SERVER_URL", server.URL, 1))
	if _, err := a.Connect(ctx, "admin", source, root); err != nil {
		t.Fatal(err)
	}
	canonical := filepath.Join(root, ".9a", "integrations", "weather.yaml")
	updated := []byte(strings.Replace(string(source), "  current:\n", "  refreshed:\n", 1))
	if err := os.WriteFile(canonical, updated, 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := a.Doctor(ctx, "admin", root, false)
	if err != nil || report.Healthy {
		t.Fatalf("stale report=%#v err=%v", report, err)
	}
	canonicalRoot, err := canonicalWorkspaceRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.New(db).ResolveWorkspaceCapability(ctx, canonicalRoot, "weather/refreshed"); err == nil {
		t.Fatal("read-only doctor changed the catalog")
	}

	report, err = a.Doctor(ctx, "admin", root, true)
	if err != nil || !report.Healthy || report.Fixed != 1 {
		t.Fatalf("fixed report=%#v err=%v", report, err)
	}
	if _, err := catalog.New(db).ResolveWorkspaceCapability(ctx, canonicalRoot, "weather/refreshed"); err != nil {
		t.Fatalf("refreshed capability: %v", err)
	}
}
