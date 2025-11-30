package pull

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	toyexec "github.com/creotiv/toy-docker/internal/exec"
)

const imagesDir = "images"

const (
	dockerHubRegistry = "registry-1.docker.io"
	dockerHubAuth     = "https://auth.docker.io/token"
)

type dockerToken struct {
	Token string `json:"token"`
}

type manifestList struct {
	Manifests []struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Platform  struct {
			Architecture string `json:"architecture"`
			OS           string `json:"os"`
		} `json:"platform"`
	} `json:"manifests"`
}

type manifest struct {
	Layers []struct {
		Digest string `json:"digest"`
	} `json:"layers"`
}

type imageRef struct {
	Registry    string
	Repository  string
	Tag         string
	DisplayName string
}

// PullImage downloads an image (any repository on Docker Hub) and flattens its
// layers into a single layer.tar inside images/<image>.
func PullImage(ref string) error {
	img, err := parseRef(ref)
	if err != nil {
		return err
	}

	outDir := filepath.Join(imagesDir, img.DisplayName)
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return fmt.Errorf("prepare image dir: %w", err)
	}

	layerPath := filepath.Join(outDir, "layer.tar")
	if _, err := os.Stat(layerPath); err == nil {
		fmt.Println("already have image:", img.DisplayName)
		return nil
	}

	token, err := fetchToken(img)
	if err != nil {
		return err
	}

	manBody, ctype, err := fetchManifest(img, token, img.Tag)
	if err != nil {
		return err
	}

	if isIndex(ctype, manBody) {
		digest, err := chooseManifestDigest(manBody)
		if err != nil {
			return err
		}
		manBody, ctype, err = fetchManifest(img, token, digest)
		if err != nil {
			return err
		}
	}

	imgManifest, err := parseImageManifest(manBody)
	if err != nil {
		return err
	}

	tmpRoot, err := os.MkdirTemp(outDir, "rootfs-")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpRoot)

	rootfs := filepath.Join(tmpRoot, "rootfs")
	if err := os.MkdirAll(rootfs, 0755); err != nil {
		return fmt.Errorf("create rootfs: %w", err)
	}

	for i, l := range imgManifest.Layers {
		if err := fetchLayer(img, token, l.Digest, rootfs); err != nil {
			return fmt.Errorf("layer %d (%s): %w", i, l.Digest, err)
		}
	}

	if err := toyexec.Run("tar", "-C", rootfs, "-cf", layerPath, "."); err != nil {
		return fmt.Errorf("pack layer: %w", err)
	}

	meta := fmt.Sprintf(`{"name":"%s","parent":null}`, img.DisplayName)
	if err := os.WriteFile(filepath.Join(outDir, "meta.json"), []byte(meta), 0644); err != nil {
		return fmt.Errorf("write meta: %w", err)
	}

	fmt.Printf("pulled: %s as %s\n", ref, img.DisplayName)
	return nil
}

func parseRef(ref string) (imageRef, error) {
	if ref == "" {
		return imageRef{}, errors.New("image reference is required")
	}

	repoPart, tag := splitRef(ref)

	if repoPart == "" {
		return imageRef{}, errors.New("image reference is invalid")
	}

	registry := dockerHubRegistry
	repo := repoPart

	parts := strings.Split(repoPart, "/")
	if len(parts) == 1 {
		repo = "library/" + parts[0]
	} else {
		if strings.Contains(parts[0], ".") || strings.Contains(parts[0], ":") || parts[0] == "localhost" {
			registry = parts[0]
			repo = strings.Join(parts[1:], "/")
		} else {
			repo = strings.Join(parts, "/")
		}
	}

	name := repo
	if registry == dockerHubRegistry && strings.HasPrefix(name, "library/") {
		name = strings.TrimPrefix(name, "library/")
	}
	name = strings.ReplaceAll(name, "/", "-")
	name = name + "-" + tag

	return imageRef{
		Registry:    registry,
		Repository:  repo,
		Tag:         tag,
		DisplayName: name,
	}, nil
}

// splitRef extracts repository and tag from a user-provided reference.
// It supports the canonical <repo>:<tag> format and a shorthand that matches
// the on-disk image name (<repo-with-dashes>-<tag>) when the tag suffix starts
// with a digit (e.g. "ubuntu-22.04").
func splitRef(ref string) (string, string) {
	repoPart := ref
	tag := "latest"

	if idx := strings.LastIndex(ref, ":"); idx != -1 && idx > strings.LastIndex(ref, "/") {
		repoPart = ref[:idx]
		tag = ref[idx+1:]
	} else if idx := strings.LastIndex(ref, "-"); idx != -1 && !strings.Contains(ref, "/") && looksLikeVersionSuffix(ref[idx+1:]) {
		repoPart = ref[:idx]
		tag = ref[idx+1:]
	}

	return repoPart, tag
}

// looksLikeVersionSuffix keeps us from misinterpreting repository names like
// "hello-world" as a repo:tag split. We only rewrite when the suffix starts
// with a digit and is otherwise alphanumeric (plus ., -, _).
func looksLikeVersionSuffix(s string) bool {
	if s == "" {
		return false
	}
	if s[0] < '0' || s[0] > '9' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') ||
			c == '.' || c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
}

func fetchToken(img imageRef) (string, error) {
	// Docker Hub supports anonymous token fetch; other registries may not need it.
	if img.Registry != dockerHubRegistry {
		return "", nil
	}

	url := fmt.Sprintf("%s?service=registry.docker.io&scope=repository:%s:pull", dockerHubAuth, img.Repository)
	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("fetch token: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token request failed: %s (%s)", resp.Status, string(body))
	}

	var t dockerToken
	if err := json.Unmarshal(body, &t); err != nil {
		return "", fmt.Errorf("decode token: %w", err)
	}

	return t.Token, nil
}

func fetchManifest(img imageRef, token string, reference string) ([]byte, string, error) {
	url := fmt.Sprintf("https://%s/v2/%s/manifests/%s", img.Registry, img.Repository, reference)

	req, _ := http.NewRequest("GET", url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.index.v1+json",
	}, ", "))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetch manifest: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("manifest request failed: %s (%s)", resp.Status, string(body))
	}

	return body, resp.Header.Get("Content-Type"), nil
}

func isIndex(contentType string, body []byte) bool {
	if strings.Contains(contentType, "manifest.list.v2") || strings.Contains(contentType, "image.index.v1") {
		return true
	}
	return bytes.Contains(body, []byte(`"manifests"`))
}

func chooseManifestDigest(body []byte) (string, error) {
	var idx manifestList
	if err := json.Unmarshal(body, &idx); err != nil {
		return "", fmt.Errorf("decode manifest index: %w", err)
	}
	if len(idx.Manifests) == 0 {
		return "", errors.New("manifest index is empty")
	}

	for _, m := range idx.Manifests {
		targetOS := runtime.GOOS
		// Images we run are always Linux, even if the host OS is macOS (running Lima).
		if targetOS != "linux" {
			targetOS = "linux"
		}
		if m.Platform.OS == targetOS && m.Platform.Architecture == runtime.GOARCH {
			return m.Digest, nil
		}
	}

	// fallback to first manifest if exact platform not found
	return idx.Manifests[0].Digest, nil
}

func parseImageManifest(body []byte) (manifest, error) {
	var m manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	if len(m.Layers) == 0 {
		return manifest{}, errors.New("manifest has no layers")
	}
	return m, nil
}

func fetchLayer(img imageRef, token, digest, dest string) error {
	url := fmt.Sprintf("https://%s/v2/%s/blobs/%s", img.Registry, img.Repository, digest)
	req, _ := http.NewRequest("GET", url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch blob: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("blob request failed: %s (%s)", resp.Status, string(body))
	}

	return extractLayer(dest, resp.Body)
}

func extractLayer(dst string, r io.Reader) error {
	gr, err := gzip.NewReader(r)
	var tr *tar.Reader
	if err == nil {
		defer gr.Close()
		tr = tar.NewReader(gr)
	} else {
		tr = tar.NewReader(r)
	}

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read layer: %w", err)
		}

		base := filepath.Base(hdr.Name)
		// Handle whiteouts for overlay semantics.
		// https://specs.opencontainers.org/image-spec/layer/#opaque-whiteout
		if strings.HasPrefix(base, ".wh.") {
			if base == ".wh..wh..opq" {
				dir := filepath.Join(dst, filepath.Dir(hdr.Name))
				entries, _ := os.ReadDir(dir)
				for _, e := range entries {
					os.RemoveAll(filepath.Join(dir, e.Name()))
				}
				continue
			}
			target := filepath.Join(dst, filepath.Dir(hdr.Name), strings.TrimPrefix(base, ".wh."))
			os.RemoveAll(target)
			continue
		}

		target := filepath.Join(dst, hdr.Name)

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)); err != nil {
				return fmt.Errorf("mkdir %s: %w", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return fmt.Errorf("mkdir for file %s: %w", target, err)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return fmt.Errorf("create file %s: %w", target, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return fmt.Errorf("write file %s: %w", target, err)
			}
			f.Close()
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return fmt.Errorf("mkdir for symlink %s: %w", target, err)
			}
			// remove existing target to avoid EEXIST
			os.RemoveAll(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return fmt.Errorf("symlink %s -> %s: %w", target, hdr.Linkname, err)
			}
		case tar.TypeLink:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return fmt.Errorf("mkdir for hardlink %s: %w", target, err)
			}
			linkTarget := filepath.Join(dst, hdr.Linkname)
			os.RemoveAll(target)
			if err := os.Link(linkTarget, target); err != nil {
				return fmt.Errorf("hardlink %s -> %s: %w", target, linkTarget, err)
			}
		default:
			// ignore other types for now (block/char/fifo)
		}
	}
}
