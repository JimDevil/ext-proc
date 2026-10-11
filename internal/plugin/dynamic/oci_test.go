package dynamic

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestParseOCIRef(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in         string
		host, repo string
		reference  string
		isDigest   bool
		wantErr    bool
	}{
		{"ghcr.io/jimdevil/extproc-plugin:v1", "ghcr.io", "jimdevil/extproc-plugin", "v1", false, false},
		{"ghcr.io/jimdevil/extproc-plugin@sha256:abc", "ghcr.io", "jimdevil/extproc-plugin", "sha256:abc", true, false},
		{"ghcr.io/jimdevil/extproc-plugin", "ghcr.io", "jimdevil/extproc-plugin", "latest", false, false},
		{"docker.io/myplugin", "docker.io", "library/myplugin", "latest", false, false},
		{"no-slash", "", "", "", false, true},
		{"host/repo@md5:abc", "", "", "", false, true},
	}
	for _, tt := range tests {
		ref, err := parseOCIRef(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Fatalf("parseOCIRef(%q): expected an error", tt.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("parseOCIRef(%q): %v", tt.in, err)
		}
		if ref.host != tt.host || ref.repo != tt.repo || ref.reference != tt.reference || ref.isDigest != tt.isDigest {
			t.Fatalf("parseOCIRef(%q) = %+v", tt.in, ref)
		}
	}
}

// tarball builds an in-memory gzipped tar layer with the given files.
func tarball(t *testing.T, files map[string]string, exec map[string]bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		mode := int64(0o644)
		if exec[name] {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractLayerAndFindExecutable(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// A traversal entry must never escape the root.
	blob := tarball(t, map[string]string{
		"plugin":        "#!/bin/sh\necho plugin\n",
		"../escape":     "nope",
		"sub/extra.txt": "data",
	}, map[string]bool{"plugin": true})
	if err := extractLayer(root, blob, "application/vnd.oci.image.layer.v1.tar+gzip"); err != nil {
		t.Fatalf("extractLayer: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "plugin")); err != nil {
		t.Fatalf("plugin file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escape")); err == nil {
		t.Fatal("path traversal escaped the extraction root")
	}

	// Conventional "plugin" file wins without an annotation.
	bin, err := findExecutable(root, "")
	if err != nil || filepath.Base(bin) != "plugin" {
		t.Fatalf("findExecutable: %q, %v", bin, err)
	}

	// Explicit annotation wins over the convention.
	if err := os.WriteFile(filepath.Join(root, "custom-bin"), []byte("#!sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin, err = findExecutable(root, "custom-bin")
	if err != nil || filepath.Base(bin) != "custom-bin" {
		t.Fatalf("findExecutable(annotation): %q, %v", bin, err)
	}

	// Ambiguous roots error out and name the annotation (fresh root without
	// the conventional "plugin" name).
	root2 := t.TempDir()
	for _, name := range []string{"bin-a", "bin-b"} {
		if err := os.WriteFile(filepath.Join(root2, name), []byte("#!sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := findExecutable(root2, ""); err == nil {
		t.Fatal("expected ambiguity error")
	}
}

func TestPickPlatform(t *testing.T) {
	t.Parallel()
	m := &imageManifest{}
	m.Manifests = []struct {
		manifestDescriptor
		Platform struct {
			Architecture string `json:"architecture"`
			OS           string `json:"os"`
			Variant      string `json:"variant"`
		} `json:"platform"`
	}{
		{Platform: struct {
			Architecture string `json:"architecture"`
			OS           string `json:"os"`
			Variant      string `json:"variant"`
		}{Architecture: "amd64", OS: "linux"}},
		{Platform: struct {
			Architecture string `json:"architecture"`
			OS           string `json:"os"`
			Variant      string `json:"variant"`
		}{Architecture: runtime.GOARCH, OS: "linux"}},
	}
	d, err := pickPlatform(m)
	if err != nil {
		t.Fatalf("pickPlatform: %v", err)
	}
	if d.Digest != m.Manifests[1].Digest {
		t.Fatalf("expected the native platform entry, got %+v", d)
	}
}
