# Toy Docker: Building and Running Containers the Hard Way

This repository is a learning project that shows the moving pieces behind a minimal Docker‑like CLI. It can pull real Docker Hub images, build simple images from a Dockerfile, and run them inside Linux namespaces with a tiny networking setup.

## What a Linux container is (and what it is not)
- A container is a regular Linux process that is isolated by namespaces (pid, net, mount, uts, ipc) and given a root filesystem to run in. It is **not** a lightweight VM.
- Isolation boundaries: separate process tree, hostname, network stack, mount table, and IPC. This project does **not** add cgroups, seccomp, or user namespaces, so resource limits and strong security hardening are out of scope.
- File system comes from an unpacked image tarball that becomes the container's root via `chroot`.
- Networking is virtual: a veth pair connects the container to a bridge on the host; iptables NAT lets traffic reach the outside world.
- Limitations: needs root, Linux only, no image caching/layering beyond a single flattened layer, no detach mode, no restart policy, no logging driver, and manual cleanup of extracted rootfs if you want to reclaim disk.

## How this toy implementation works
- Image pulling: `toy-docker pull <image[:tag]>` talks to Docker Hub, fetches the manifest that matches the host OS/arch, downloads each layer, applies whiteouts, and repacks everything into a single `images/<name>/layer.tar` with a `meta.json`.
- Image building: `toy-docker build <Dockerfile> <name>` supports `FROM`, `RUN`, and `COPY`. It untars the parent image layer, executes `RUN` commands inside that rootfs using `systemd-nspawn`, copies files for `COPY`, then tars the result as a new single-layer image.
- Running a container: `toy-docker run [-v host:cont;...] [-p host:cont;...] <image> [cmd...]`
  - Extracts `images/<image>/layer.tar` into `containers/<cid>/rootfs`.
  - Creates a bridge `toy0` (10.200.0.1/24) if missing, a veth pair, and moves one end into the container netns.
  - Uses `unshare --pid --net --ipc --uts --mount --mount-proc` to start a new namespace and re-exec the binary as `init`.
  - Inside `init`: bind-mounts the rootfs, mounts `/proc`, bind-mounts requested volumes, configures `eth0` with the provided IP, sets hostname, and finally `chroot`s to run the requested command.
  - Port forwarding is iptables DNAT from host ports to the container IP. Loopback is enabled inside the netns.

## Prerequisites
- Linux host with root access (needed for `unshare`, network setup, iptables, and mounts).
- Tools: `tar`, `iptables`, `ip` (iproute2), `systemd-nspawn`, `unshare`, and `bash`.
- On macOS, run inside a Linux VM. The repo includes a Lima config: `limactl start toy-docker-linux.yaml` then `limactl shell toy-docker-linux`.

## Quickstart
```bash
# inside the Linux environment
go build -o toy-docker ./cmd/toy-docker

# pull a base image (flattens to images/ubuntu-22.04/layer.tar)
# both ubuntu:22.04 and the shorthand ubuntu-22.04 work; the latter matches the on-disk name
sudo ./toy-docker pull ubuntu:22.04

# build an example image that adds curl
sudo ./toy-docker build Dockerfiles/curl.Dockerfile curl-ubuntu

# run a shell in the built image with a bind mount and a port map
sudo ./toy-docker run -p 8080:80 curl-ubuntu /bin/curl https://google.com
```

## CLI reference
- `pull <image[:tag]>` — fetch from Docker Hub (or custom registry in the ref) and store under `images/<name>/layer.tar`. Skips if already present.
- `build <Dockerfile> <image-name>` — supports `FROM`, `RUN`, `COPY`. Uses the parent image already under `images/`. Writes `images/<image-name>/layer.tar` plus `meta.json`.
- `run [-v host:cont;...] [-p host:cont;...] <image> [cmd...]` — extracts the image layer to `containers/<cid>/rootfs`, sets up namespaces, bridge/veth networking, NAT for ports, mounts volumes, and runs the command via `chroot`. Volumes and ports are semicolon-separated.
- `images` — print stored images' metadata.
- `init` — internal helper invoked during `run` after `unshare` to finish namespace setup (not for direct use).

## Image layout and build inputs
- Pulled/built images live under `images/<name>/` with:
  - `layer.tar` — a single, flattened rootfs layer.
  - `meta.json` — `{ "name": "...", "parent": "<base or null>" }`.
- Example Dockerfile: `Dockerfiles/curl.Dockerfile`:
  ```Dockerfile
  FROM ubuntu-22.04
  RUN apt-get update
  RUN apt-get install -y curl
  ```
  After pulling `ubuntu:22.04`, build it with `toy-docker build Dockerfiles/curl.Dockerfile curl-ubuntu`.

## Limitations and cleanup notes
- No cgroups, capabilities drop, or seccomp — processes run with host privileges inside the namespace.
- Networking is basic: bridge `toy0`, static IP allocation, and iptables rules are appended (not cleaned automatically).
- Containers are foreground only; stop by exiting the process. Rootfs extraction defaults to `/tmp/toy-docker/containers` to avoid shared-mount permission issues (override with `TOY_DOCKER_CONTAINERS=<path>`). Extracted rootfs stays on disk until manually removed.
- Image format is simplified (single layer per image); only a subset of Dockerfile instructions is supported.
