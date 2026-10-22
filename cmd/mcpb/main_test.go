package main

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/mattoddie/proxmox-mcp/internal/js"
)

func write(t *testing.T, file, body string) string {
	t.Helper()
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return file
}

// readManifest reads the committed manifest. The Docker build only copies the Go sources, so
// tests that need it skip there; CI runs them outside Docker.
func readManifest(t *testing.T) *js.Obj {
	t.Helper()
	data, err := os.ReadFile("../../mcpb/manifest.json")
	if os.IsNotExist(err) {
		t.Skip("mcpb/manifest.json is not available")
	}
	if err != nil {
		t.Fatal(err)
	}
	m, err := parseManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPack(t *testing.T) {
	dir := t.TempDir()
	bin := write(t, filepath.Join(dir, "proxmox-mcp.exe"), "binary")
	license := write(t, filepath.Join(dir, "LICENSE"), "MIT")
	write(t, filepath.Join(dir, "icon.png"), "png")
	write(t, filepath.Join(dir, "manifest.json"), `{"manifest_version":"0.3","name":"proxmox-mcp","version":"0.0.0-dev","icon":"icon.png",
		"server":{"type":"binary","entry_point":"server/proxmox-mcp"},"compatibility":{"platforms":["darwin","win32","linux"]}}`)
	out := filepath.Join(dir, "out", "x.mcpb")
	if err := pack("1.2.3-rc.1", "windows", bin, out, dir, license); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var names []string
	files := map[string]*zip.File{}
	for _, f := range zr.File {
		names = append(names, f.Name)
		files[f.Name] = f
	}
	if want := []string{"manifest.json", "icon.png", "LICENSE", "server/proxmox-mcp.exe"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("entries: %v, want %v", names, want)
	}
	if mode := files["server/proxmox-mcp.exe"].Mode().Perm(); mode != 0o755 {
		t.Errorf("binary mode %v", mode)
	}
	r, _ := files["manifest.json"].Open()
	data, _ := io.ReadAll(r)
	m, err := parseManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	if v := m.Get("version"); v != "1.2.3-rc.1" {
		t.Errorf("version %v", v)
	}
	if p := js.Stringify(m.ObjAt("compatibility").Get("platforms"), ""); p != `["win32"]` {
		t.Errorf("platforms %s", p)
	}
	if e := m.ObjAt("server").Get("entry_point"); e != "server/proxmox-mcp.exe" {
		t.Errorf("entry_point %v", e)
	}

	for _, bad := range [][2]string{{"v1.2.3", "linux"}, {"1.2", "linux"}, {"1.2.3", "plan9"}} {
		if err := pack(bad[0], bad[1], bin, out, dir, license); err == nil {
			t.Errorf("pack(%q, %q) succeeded", bad[0], bad[1])
		}
	}
}

// The manifest's settings, at their defaults, must start the server read-only: every variable
// it sets comes from a user_config entry that exists.
func TestDefaultEnv(t *testing.T) {
	m := readManifest(t)
	env, err := defaultEnv(m)
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"MCP_TRANSPORT": "stdio", "PROXMOX_URL": samples["url"], "PROXMOX_ALLOW_WRITES": "false",
		"PROXMOX_ALLOW_DELETES": "false", "PROXMOX_ALLOW_EXEC": "false", "PROXMOX_VERIFY_SSL": "", "PROXMOX_AUDIT_LOG": ""} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}
	m.Set("user_config", js.NewObj())
	if _, err := defaultEnv(m); err == nil {
		t.Error("a reference to a missing user_config entry was accepted")
	}
}

// The committed manifest lists the tools a default install registers.
func TestManifestUpToDate(t *testing.T) {
	m := readManifest(t)
	want := js.Stringify(m, "  ")
	if err := generate(m); err != nil {
		t.Fatal(err)
	}
	if got := js.Stringify(m, "  "); got != want {
		t.Error("mcpb/manifest.json is out of date: run make docs")
	}
}
