# Building the Arcane Windows image from Linux

Arcane's Windows build has two stages:

1. **goreleaser** cross-compiles `arcane.exe`, exactly as it does for every other
   platform — the `arcane` build in `.goreleaser.yaml` lists `windows` in its
   `goos` and carries the `timetzdata` tag. The frontend is built once by the
   `before:` hook and embedded into every target, so nothing Windows-specific is
   needed to produce the binary.
2. **`docker/Dockerfile.windows`** turns that binary into an image. It is
   COPY-only — the same shape as `docker/Dockerfile-static` for Linux — and
   `docker build` can only run it **on a Windows container host**, because a Linux
   daemon cannot materialise Windows base layers.

This tool replaces stage 2 when no Windows host is available. A container image
is only a manifest, a config and some layers, so it builds the layer and the
config directly and talks to the registry itself. The result is a normal Windows
image: `docker pull` and `docker run` on Windows Server treat it like any other.

## Usage

```sh
# stage 1: cross-compile on Linux (or take the .exe from a release build)
just build single frontend
cd backend && CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
  go build -tags timetzdata -trimpath -o ../dist-windows/arcane.exe ./cmd/main.go
cd ..

# stage 2: package and push, from Linux
cd docker/windows-image
GOWORK=off go run . \
  -exe ../../dist-windows/arcane.exe \
  -version "$(jq -r .version ../../.arcane.json)" \
  -tag registry.example.com/arcane/arcane:2.12.0-windows-ltsc2022 \
  -push
```

`GOWORK=off` is needed because this is a standalone module and the repository
root has a `go.work` that does not list it.

Flags worth knowing:

| flag | purpose |
| --- | --- |
| `-base` | Windows base image. **Its tag must match the target host's build**: `ltsc2019` → Server 2019, `ltsc2022` → Server 2022, `ltsc2025` → Server 2025. A mismatch means the container will not start under process isolation. |
| `-save` | Also write a `docker load`-able tarball, for hosts that cannot reach the registry. |
| `-no-volume` | Omit the image-declared `VOLUME`. Windows cannot populate a volume from image content, and image-declared volumes are poorly supported there; use this with a host bind mount (`-v C:\ProgramData\arcane:C:\arcane\data`). |

Credentials come from the usual Docker config (`~/.docker/config.json` or
`DOCKER_CONFIG`).

## Why the layer format matters

A Windows layer is **not** an ordinary rootfs tar, and getting this wrong is the
only hard part of the job. On the Windows host, moby's graphdriver
(`daemon/graphdriver/windows`) reads each tar entry through
`backuptar.FileInfoFromHeader`, which reconstructs Win32 metadata from `MSWINDOWS.*`
pax records, and replays it through hcsshim's legacy layer writer, which stages
the files and finally calls `ImportLayer`.

A layer therefore needs all of:

- **paths rooted at `Files/`** — `Files/arcane/arcane.exe` becomes
  `C:\arcane\arcane.exe` in the container.
- **a top-level `Hives` directory** — it holds registry deltas, and every
  Windows layer carries the directory even when it has none.
- **`MSWINDOWS.fileattr` pax records** — `16` for directories, `32` for files.
- **`MSWINDOWS.rawsd` pax records** — the base64 binary security descriptor.
- **directory names without a trailing slash**, matching what the platform's own
  images do.

Omitting the metadata produces a layer that pushes and pulls happily and then
fails at import with:

```
failed to register layer: re-exec error: exit status 1: output:
hcsshim::ImportLayer failed in Win32: The system cannot find the path specified. (0x3)
```

To inspect a known-good reference layer, pull a small Microsoft image and read
its pax records:

```sh
crane blob mcr.microsoft.com/oss/kubernetes/pause@<windows-layer-digest> | tar -tvf -
```

## Other Windows-specific behaviour

- **`-tags timetzdata` is required** (already set in the builder Dockerfile):
  Windows ships no system zoneinfo, so `time.LoadLocation` fails without
  embedded tzdata.
- **Do not set `PUID`/`PGID`.** `os.Geteuid()` returns `-1` on Windows, so
  `runtime_identity.go` takes its "not root, continue as current user" branch
  and never chowns. Setting them sends startup down the Unix ownership path.
- **Path defaults.** `PROJECTS_DIRECTORY` and `TEMPLATES_DIRECTORY` default to
  `/app/data/...`, and the `buildsDirectory` setting to `/builds`. None of these
  are absolute on Windows (`filepath.IsAbs` wants a drive letter), so they
  resolve against the current drive root and land outside the data volume. This
  tool pins all of them under `C:\arcane\data`.
- **Image patching is unavailable** and returns
  `ErrPatchUnsupportedPlatform`. Copa rebuilds *Linux* image layers through
  BuildKit, which a Windows engine has no worker for; see
  `backend/internal/imagepatch/copa_windows.go`.

## Running it

```powershell
docker run -d --name arcane --restart=always `
  -p 3552:3552 `
  -v \\.\pipe\docker_engine:\\.\pipe\docker_engine `
  -v arcane-data:C:\arcane\data `
  -e APP_URL=http://<host>:3552 `
  -e ENCRYPTION_KEY=<at least 32 characters> `
  registry.example.com/arcane/arcane:2.12.0-windows-ltsc2022
```

`ENCRYPTION_KEY` is mandatory: bootstrap panics in production on a passphrase
shorter than 32 characters. `APP_URL` must match the port you publish.

If publishing a port fails with `hnsCall failed in Win32: The specified port
already exists`, that is host HNS state rather than anything in the image —
check `netsh int ipv4 show excludedportrange protocol=tcp` for a Hyper-V
reservation covering the port, and clear stale endpoints with
`Restart-Service hns -Force` after stopping the Docker service.
