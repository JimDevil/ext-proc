package dynamic

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Minimal OCI/Docker registry v2 client for anonymous pulls of PUBLIC plugin
// images. Auth follows the challenge flow (WWW-Authenticate: Bearer ...),
// which covers ghcr.io, docker.io and any registry behind the same scheme.
// Private registries: pre-pull the image with an external tool and reference
// it via file:// instead.

const (
	ociExecAnnotation = "org.opencontainers.image.exec"

	mtManifestOCI    = "application/vnd.oci.image.manifest.v1+json"
	mtManifestDocker = "application/vnd.docker.distribution.manifest.v2+json"
	mtIndexOCI       = "application/vnd.oci.image.index.v1+json"
	mtIndexDocker    = "application/vnd.docker.distribution.manifest.list.v2+json"
)

var manifestAccept = strings.Join([]string{mtManifestOCI, mtManifestDocker, mtIndexOCI, mtIndexDocker}, ", ")

// ociRef is a parsed plugin image reference.
type ociRef struct {
	host      string // ghcr.io, docker.io, registry:port ...
	repo      string // owner/name
	reference string // tag or "sha256:..."
	isDigest  bool
}

// parseOCIRef parses "host/repo[:tag]|@sha256:digest" (the oci:// prefix was
// stripped by the caller).
func parseOCIRef(s string) (ociRef, error) {
	ref := ociRef{}
	host, rest, ok := strings.Cut(s, "/")
	if !ok || host == "" || rest == "" {
		return ref, fmt.Errorf("invalid image reference %q (want host/repo:tag)", s)
	}
	ref.host = host

	name, digest, hasDigest := strings.Cut(rest, "@")
	if hasDigest {
		if !strings.HasPrefix(digest, "sha256:") {
			return ref, fmt.Errorf("unsupported digest %q (only sha256:...)", digest)
		}
		ref.repo, ref.reference, ref.isDigest = name, digest, true
	} else {
		name, tag, hasTag := strings.Cut(name, ":")
		if !hasTag {
			tag = "latest"
		}
		ref.repo, ref.reference = name, tag
	}

	// docker.io shorthand: single-path repos live under library/.
	if ref.host == "docker.io" && !strings.Contains(ref.repo, "/") {
		ref.repo = "library/" + ref.repo
	}
	return ref, nil
}

// manifestDescriptor points at one manifest inside an index.
type manifestDescriptor struct {
	Digest    string `json:"digest"`
	MediaType string `json:"mediaType"`
}

// imageManifest is the union of OCI/Docker v2 image manifest and index fields.
type imageManifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType"`
	Annotations   map[string]string `json:"annotations"`
	Manifests     []struct {
		manifestDescriptor
		Platform struct {
			Architecture string `json:"architecture"`
			OS           string `json:"os"`
			Variant      string `json:"variant"`
		} `json:"platform"`
	} `json:"manifests"`
	Config struct {
		Digest string `json:"digest"`
	} `json:"config"`
	Layers []struct {
		Digest    string `json:"digest"`
		MediaType string `json:"mediaType"`
		Size      int64  `json:"size"`
	} `json:"layers"`
}

// registryClient does challenge-authenticated anonymous registry calls.
type registryClient struct {
	http  *http.Client
	token string
}

func newRegistryClient() *registryClient {
	return &registryClient{http: &http.Client{Timeout: 30 * time.Second}}
}

// fetch performs a GET with bearer-challenge retry; used for manifests and
// blobs alike. pathURL is relative to /v2/<repo>/.
func (c *registryClient) fetch(ref ociRef, pathURL, accept string) ([]byte, error) {
	endpoint := fmt.Sprintf("https://%s/v2/%s/%s", ref.host, ref.repo, pathURL)
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequest(http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, err
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			return body, nil
		case resp.StatusCode == http.StatusUnauthorized && attempt == 0:
			if err := c.authenticate(resp.Header.Get("WWW-Authenticate"), ref.repo); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("GET %s: HTTP %d", pathURL, resp.StatusCode)
		}
	}
	return nil, fmt.Errorf("GET %s: unauthorized", pathURL)
}

// authenticate resolves a 401 challenge into a bearer token.
func (c *registryClient) authenticate(challenge, repo string) error {
	if !strings.HasPrefix(challenge, "Bearer ") {
		return fmt.Errorf("unsupported auth challenge: %q (anonymous public pulls only)", challenge)
	}
	params := parseChallenge(challenge[len("Bearer "):])
	realm := params["realm"]
	if realm == "" {
		return fmt.Errorf("auth challenge without realm: %q", challenge)
	}
	q := url.Values{}
	q.Set("service", params["service"])
	q.Set("scope", "repository:"+repo+":pull")
	req, err := http.NewRequest(http.MethodGet, realm+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("token endpoint %s: HTTP %d", realm, resp.StatusCode)
	}
	var out struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	c.token = out.Token
	if c.token == "" {
		c.token = out.AccessToken
	}
	if c.token == "" {
		return fmt.Errorf("token endpoint %s returned no token", realm)
	}
	return nil
}

// parseChallenge parses `k="v",k2="v2"` pairs from a Bearer challenge.
func parseChallenge(s string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	return out
}

// resolveOCI pulls the plugin image (digest-addressed cache) and returns the
// extracted executable path. Tags are resolved on every load — a cached
// digest directory makes repeat loads a single manifest GET.
func resolveOCI(imageRef string, cacheDir string, logger *slog.Logger) (string, error) {
	ref, err := parseOCIRef(imageRef)
	if err != nil {
		return "", err
	}
	c := newRegistryClient()

	// Manifest (follow an index/manifest list to the local platform).
	manifestBytes, err := c.fetch(ref, "manifests/"+ref.reference, manifestAccept)
	if err != nil {
		return "", err
	}
	var manifest imageManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return "", fmt.Errorf("parse manifest: %w", err)
	}
	if isIndex(manifest.MediaType) {
		descriptor, err := pickPlatform(&manifest)
		if err != nil {
			return "", err
		}
		manifestBytes, err = c.fetch(ref, "manifests/"+descriptor.Digest, manifestAccept)
		if err != nil {
			return "", err
		}
		manifest = imageManifest{}
		if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
			return "", fmt.Errorf("parse platform manifest: %w", err)
		}
	}
	digest := sha256Hex(manifestBytes)

	dir := filepath.Join(cacheDir, digest)
	marker := filepath.Join(dir, ".complete")
	if _, err := os.Stat(marker); err == nil {
		return findExecutable(dir, manifest.Annotations[ociExecAnnotation])
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	for _, layer := range manifest.Layers {
		blob, err := c.fetch(ref, "blobs/"+layer.Digest, "")
		if err != nil {
			return "", fmt.Errorf("fetch layer %s: %w", layer.Digest, err)
		}
		if err := extractLayer(dir, blob, layer.MediaType); err != nil {
			return "", fmt.Errorf("extract layer %s: %w", layer.Digest, err)
		}
	}
	if err := os.WriteFile(marker, []byte(digest+"\n"), 0o644); err != nil {
		return "", err
	}
	logger.Info("plugin image pulled",
		slog.String("image", imageRef),
		slog.String("digest", digest),
		slog.Int("layers", len(manifest.Layers)))
	return findExecutable(dir, manifest.Annotations[ociExecAnnotation])
}

// isIndex reports whether the manifest is an index/manifest list.
func isIndex(mediaType string) bool {
	return mediaType == mtIndexOCI || mediaType == mtIndexDocker
}

// pickPlatform selects the entry matching linux/GOARCH (amd64 fallback).
func pickPlatform(m *imageManifest) (manifestDescriptor, error) {
	fallback := -1
	for i, d := range m.Manifests {
		p := d.Platform
		if p.OS != "" && p.OS != "linux" {
			continue
		}
		if p.Architecture == runtime.GOARCH {
			return d.manifestDescriptor, nil
		}
		if p.Architecture == "amd64" {
			fallback = i
		}
	}
	if fallback >= 0 {
		return m.Manifests[fallback].manifestDescriptor, nil
	}
	return manifestDescriptor{}, fmt.Errorf("no manifest entry for linux/%s", runtime.GOARCH)
}

// extractLayer unpacks one layer blob into root (tar, optionally gzipped).
func extractLayer(root string, blob []byte, mediaType string) error {
	r := io.Reader(bytes.NewReader(blob))
	if strings.Contains(mediaType, "gzip") ||
		(len(blob) > 2 && blob[0] == 0x1f && blob[1] == 0x8b) {
		gz, err := gzip.NewReader(r)
		if err != nil {
			return err
		}
		defer gz.Close()
		r = gz
	}
	tr := tar.NewReader(r)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(header.Name)
		if name == "." || strings.HasPrefix(name, "..") || filepath.IsAbs(name) {
			continue // never escape the extraction root
		}
		target := filepath.Join(root, name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, header.FileInfo().Mode())
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(f, tr)
			closeErr := f.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			// symlinks/hardlinks/devices are not used by plugin images
		}
	}
}

// sha256Hex hashes manifest bytes into the cache directory name.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
