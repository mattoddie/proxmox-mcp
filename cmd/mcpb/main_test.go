package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
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

// serverJSON is the part of server.json (the MCP Registry entry) that the tests check.
type serverJSON struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Version     string `json:"version"`
	Packages    []struct {
		RegistryType string `json:"registryType"`
		Identifier   string `json:"identifier"`
		FileSha256   string `json:"fileSha256"`
		Transport    struct {
			Type string `json:"type"`
		} `json:"transport"`
		EnvironmentVariables []struct {
			Name       string `json:"name"`
			Value      string `json:"value"`
			Default    string `json:"default"`
			Format     string `json:"format"`
			IsRequired bool   `json:"isRequired"`
			IsSecret   bool   `json:"isSecret"`
		} `json:"environmentVariables"`
	} `json:"packages"`
}

// notInImage lists the bundle's settings that the Docker package leaves out, with the reason.
var notInImage = map[string]string{
	"PROXMOX_CA_FILE": "a file on the user's computer isn't visible inside the container",
}

// server.json must offer the same settings as the bundle, so the registry entry doesn't drift
// from the manifest (which TestManifestUpToDate checks against the server), and list a bundle
// for every platform `make mcpb` builds. The release stamps the version and checksums in.
func TestServerJSON(t *testing.T) {
	m := readManifest(t)
	data, err := os.ReadFile("../../server.json")
	if err != nil {
		t.Fatal(err)
	}
	var s serverJSON
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	if s.Name != "io.github.mattoddie/proxmox-mcp" {
		t.Errorf("name %q", s.Name)
	}
	if n := len([]rune(s.Description)); n == 0 || n > 100 {
		t.Errorf("description is %d characters; the registry allows 1 to 100", n)
	}
	if s.Version != "0.0.0-dev" {
		t.Errorf("version %q: keep 0.0.0-dev, which `make server-json` replaces", s.Version)
	}

	makefile, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	platforms := regexp.MustCompile(`(?m)^MCPB_PLATFORMS := (.+)$`).FindSubmatch(makefile)
	if platforms == nil {
		t.Fatal("no MCPB_PLATFORMS in the Makefile")
	}
	var wantBundles, bundles []string
	for _, p := range strings.Fields(string(platforms[1])) {
		wantBundles = append(wantBundles, "https://github.com/mattoddie/proxmox-mcp/releases/download/v0.0.0-dev/proxmox-mcp-0.0.0-dev-"+strings.ReplaceAll(p, "/", "-")+".mcpb")
	}

	userConfig := m.ObjAt("user_config")
	manifestEnv := m.ObjAt("server").ObjAt("mcp_config").ObjAt("env")
	images := 0
	for _, p := range s.Packages {
		if p.Transport.Type != "stdio" {
			t.Errorf("%s: transport %q, want stdio", p.Identifier, p.Transport.Type)
		}
		switch p.RegistryType {
		case "mcpb":
			bundles = append(bundles, p.Identifier)
			if len(p.FileSha256) != 64 {
				t.Errorf("%s: fileSha256 %q", p.Identifier, p.FileSha256)
			}
		case "oci":
			images++
			if p.Identifier != "ghcr.io/mattoddie/proxmox-mcp:0.0.0-dev" {
				t.Errorf("image %q", p.Identifier)
			}
			seen := map[string]bool{}
			for _, v := range p.EnvironmentVariables {
				seen[v.Name] = true
				if v.Name == "MCP_TRANSPORT" {
					// The image defaults to HTTP; the registry's clients start it over stdio.
					if v.Value != "stdio" {
						t.Errorf("MCP_TRANSPORT value %q, want stdio", v.Value)
					}
					continue
				}
				if reason, ok := notInImage[v.Name]; ok {
					t.Errorf("%s is in the image's settings, but %s", v.Name, reason)
					continue
				}
				ref := userConfigRef.FindStringSubmatch(js.Str(manifestEnv.Get(v.Name)))
				if ref == nil {
					t.Errorf("%s isn't a setting in mcpb/manifest.json", v.Name)
					continue
				}
				setting := userConfig.ObjAt(ref[1])
				if v.IsRequired != setting.Truthy("required") {
					t.Errorf("%s: isRequired %v, but the manifest's required is %v", v.Name, v.IsRequired, setting.Truthy("required"))
				}
				if v.IsSecret != setting.Truthy("sensitive") {
					t.Errorf("%s: isSecret %v, but the manifest's sensitive is %v", v.Name, v.IsSecret, setting.Truthy("sensitive"))
				}
				if setting.Str("type") == "boolean" && v.Format != "boolean" {
					t.Errorf("%s: format %q, want boolean", v.Name, v.Format)
				}
				def := ""
				if setting.Defined("default") {
					def = fmt.Sprint(setting.Get("default"))
				}
				if def != v.Default {
					t.Errorf("%s: default %q, but the manifest's is %q", v.Name, v.Default, def)
				}
			}
			for _, name := range manifestEnv.Keys() {
				if _, skip := notInImage[name]; !skip && !seen[name] {
					t.Errorf("%s is a setting in mcpb/manifest.json but not in server.json", name)
				}
			}
		default:
			t.Errorf("unexpected package type %q", p.RegistryType)
		}
	}
	if images != 1 {
		t.Errorf("%d Docker image packages, want 1", images)
	}
	if !reflect.DeepEqual(bundles, wantBundles) {
		t.Errorf("bundles:\n%s\nwant (from the Makefile):\n%s", strings.Join(bundles, "\n"), strings.Join(wantBundles, "\n"))
	}

	dockerfile, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dockerfile), `io.modelcontextprotocol.server.name="`+s.Name+`"`) {
		t.Error("the Dockerfile has no io.modelcontextprotocol.server.name label for " + s.Name)
	}
}
