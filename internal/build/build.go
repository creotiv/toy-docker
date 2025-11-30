package build

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/creotiv/toy-docker/internal/exec"
)

type Meta struct {
	Name   string `json:"name"`
	Parent string `json:"parent"`
}

const imagesDir = "images"

// We dont use overlayfs to simplify example
func BuildImage(dockerfile, name string) error {
	parent, cmds, err := parseDockerfile(dockerfile)
	if err != nil {
		return err
	}

	parentDir := imagesDir + "/" + parent

	tmp, err := os.MkdirTemp("", "toy-docker-build-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	// unpack base image
	exec.MustRun("[fs] unpack base image", "tar", "-C", tmp, "-xf", parentDir+"/layer.tar")

	// apply RUN/COPY
	for _, c := range cmds {
		if strings.HasPrefix(c, "RUN ") {
			script := strings.TrimPrefix(c, "RUN ")
			fmt.Println(">>> RUN", script)
			// This is how old Docker originally worked
			// It run command against temporary rootfs
			exec.MustRun("[fs] run command inside container", "systemd-nspawn", "-D", tmp, "/bin/bash", "-c", script)
		}
		if strings.HasPrefix(c, "COPY ") {
			parts := strings.SplitN(strings.TrimPrefix(c, "COPY "), " ", 2)
			src := parts[0]
			dst := parts[1]
			exec.MustRun("[fs] mkdir", "mkdir", "-p", tmp+"/"+dst)
			exec.MustRun("[fs] copy file", "cp", "-r", src, tmp+"/"+dst)
		}
	}

	// new layer
	// pack everything togather and save in new image
	// we dont use overlayfs here
	outDir := imagesDir + "/" + name
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return fmt.Errorf("create image dir: %w", err)
	}
	exec.MustRun("[fs] pack new layer", "tar", "-C", tmp, "-cf", outDir+"/layer.tar", ".")

	meta := Meta{Name: name, Parent: parent}
	m, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(outDir+"/meta.json", m, 0644); err != nil {
		return fmt.Errorf("write meta: %w", err)
	}

	fmt.Println("image built:", name)
	return nil
}

func ListImages() error {
	ents, _ := os.ReadDir(imagesDir)
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		metaPath := imagesDir + "/" + e.Name() + "/meta.json"
		if _, err := os.Stat(metaPath); err != nil {
			continue
		}
		b, _ := os.ReadFile(metaPath)
		fmt.Println(string(b))
	}
	return nil
}

func parseDockerfile(path string) (string, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil, fmt.Errorf("read dockerfile: %w", err)
	}

	var parent string
	var cmds []string
	for _, raw := range strings.Split(string(data), "\n") {
		l := strings.TrimSpace(raw)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		switch {
		case strings.HasPrefix(l, "FROM "):
			parent = strings.TrimSpace(strings.TrimPrefix(l, "FROM "))
		case strings.HasPrefix(l, "RUN "):
			cmds = append(cmds, "RUN "+strings.TrimPrefix(l, "RUN "))
		case strings.HasPrefix(l, "COPY "):
			cmds = append(cmds, "COPY "+strings.TrimPrefix(l, "COPY "))
		default:
			return "", nil, fmt.Errorf("unknown instruction: %s", l)
		}
	}
	if parent == "" {
		return "", nil, fmt.Errorf("dockerfile missing FROM")
	}
	return parent, cmds, nil
}
