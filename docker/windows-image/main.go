// Command windows-image packages a cross-compiled arcane.exe as a Windows
// container image, from Linux, without a Windows container host.
//
// Stage B of the Windows build (docker/Dockerfile.windows) needs a Windows
// daemon, because a Linux daemon cannot materialise Windows base layers. This
// tool is an alternative to stage B: an image is only a manifest, a config and
// a set of layers, so it builds the layer and rewrites the config directly
// against the registry. See README.md for the layer format details, which are
// the part that is easy to get wrong.
//
// Usage:
//
//	go run . -exe ../../dist-windows/arcane.exe \
//	         -tag registry.example.com/arcane/arcane:2.12.0-windows-ltsc2022 -push
package main

import (
	"archive/tar"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// Win32 file attributes, as carried in the MSWINDOWS.fileattr pax record.
const (
	fileAttributeDirectory = 16
	fileAttributeArchive   = 32
)

// Raw security descriptors for the entries we add, base64 of the binary SD, as
// carried in the MSWINDOWS.rawsd pax record.
//
// These are the descriptors Microsoft's own images use (harvested from
// mcr.microsoft.com/oss/kubernetes/pause): Administrators and SYSTEM get full
// control, Users get read and execute. Arcane runs as ContainerAdministrator,
// so it can write the data directory. A layer whose entries carry no
// descriptor at all is imported with whatever ACL the staging file happened to
// get, so it is better to be explicit.
const (
	dirSecurityDescriptor  = "AQAEgBQAAAAkAAAAAAAAADAAAAABAgAAAAAABSAAAAAgAgAAAQEAAAAAAAUSAAAAAgCoAAcAAAAAAxgA/wEfAAECAAAAAAAFIAAAACACAAAAAxQA/wEfAAEBAAAAAAAFEgAAAAAAGAD/AR8AAQIAAAAAAAUgAAAAIAIAAAALFAAAAAAQAQEAAAAAAAMAAAAAAAMYAKkAEgABAgAAAAAABSAAAAAhAgAAAAIYAAQAAAABAgAAAAAABSAAAAAhAgAAAAIYAAIAAAABAgAAAAAABSAAAAAhAgAA"
	fileSecurityDescriptor = "AQAEgBQAAAAkAAAAAAAAADAAAAABAgAAAAAABSAAAAAgAgAAAQEAAAAAAAUSAAAAAgBMAAMAAAAAABgA/wEfAAECAAAAAAAFIAAAACACAAAAABQA/wEfAAEBAAAAAAAFEgAAAAAAGACpABIAAQIAAAAAAAUgAAAAIQIAAA=="
)

func main() {
	var (
		exePath  = flag.String("exe", "dist-windows/arcane.exe", "path to the cross-compiled arcane.exe")
		baseRef  = flag.String("base", "mcr.microsoft.com/windows/nanoserver:ltsc2022", "Windows base image; its tag must match the target host's build")
		tagRef   = flag.String("tag", "", "image reference to produce")
		version  = flag.String("version", "dev", "value for the org.opencontainers.image.version label")
		push     = flag.Bool("push", false, "push the image to -tag")
		saveTar  = flag.String("save", "", "also write a docker-loadable tarball here")
		noVolume = flag.Bool("no-volume", false, "omit the image-declared VOLUME (Windows cannot populate a volume from image content)")
	)
	flag.Parse()
	if *tagRef == "" {
		fatal(fmt.Errorf("-tag is required"))
	}

	tag, err := name.NewTag(*tagRef)
	must(err)

	base, err := remote.Image(mustRef(*baseRef), remote.WithPlatform(v1.Platform{OS: "windows", Architecture: "amd64"}))
	must(err)
	baseCfg, err := base.ConfigFile()
	must(err)
	fmt.Printf("base %s: os=%s os.version=%s arch=%s\n", *baseRef, baseCfg.OS, baseCfg.OSVersion, baseCfg.Architecture)

	layerPath, err := buildLayer(*exePath)
	must(err)
	defer os.Remove(layerPath)

	layer, err := tarball.LayerFromFile(layerPath)
	must(err)

	img, err := mutate.AppendLayers(base, layer)
	must(err)

	// Derive the new config from the APPENDED image, never from the base:
	// AppendLayers adds the new layer's diff_id to rootfs, and a config cloned
	// from the base would not have it. Docker accepts such a push and then
	// fails the pull with "layers from manifest don't match image
	// configuration".
	appended, err := img.ConfigFile()
	must(err)
	cfg := appended.DeepCopy()
	cfg.Config.User = "ContainerAdministrator"
	cfg.Config.WorkingDir = `C:\arcane`
	cfg.Config.Entrypoint = []string{`C:\arcane\arcane.exe`}
	cfg.Config.Cmd = nil
	cfg.Config.Env = append(cfg.Config.Env,
		"PORT=3552",
		"ENVIRONMENT=production",
		"ARCANE_IN_CONTAINER=true",
		// The Windows engine speaks over a named pipe, not a unix socket.
		"DOCKER_HOST=npipe:////./pipe/docker_engine",
		// Every path below defaults to something that is not absolute on
		// Windows, so it would resolve against the current drive root and land
		// outside the data volume. Setting BUILDS_DIRECTORY additionally makes
		// NormalizeBuildsDirectory skip its normalisation pass.
		`DOCKER_CONFIG=C:\arcane\data\.docker`,
		`PROJECTS_DIRECTORY=C:\arcane\data\projects`,
		`TEMPLATES_DIRECTORY=C:\arcane\data\templates`,
		`BUILDS_DIRECTORY=C:\arcane\data\builds`,
	)
	cfg.Config.ExposedPorts = map[string]struct{}{"3552/tcp": {}}
	if !*noVolume {
		cfg.Config.Volumes = map[string]struct{}{`C:\arcane\data`: {}}
	}
	if cfg.Config.Labels == nil {
		cfg.Config.Labels = map[string]string{}
	}
	cfg.Config.Labels["com.getarcaneapp.arcane"] = "true"
	cfg.Config.Labels["org.opencontainers.image.title"] = "Arcane (Windows)"
	cfg.Config.Labels["org.opencontainers.image.version"] = *version
	cfg.Config.Labels["org.opencontainers.image.base.name"] = *baseRef
	img, err = mutate.ConfigFile(img, cfg)
	must(err)

	must(verify(img))

	digest, err := img.Digest()
	must(err)
	fmt.Println("image digest:", digest)

	if *saveTar != "" {
		fmt.Println("writing tarball:", *saveTar)
		must(tarball.WriteToFile(*saveTar, tag, img))
	}
	if *push {
		fmt.Println("pushing", tag.Name())
		must(remote.Write(tag, img, remote.WithAuthFromKeychain(authn.DefaultKeychain)))
		fmt.Println("pushed:", tag.Name(), digest)
	}
}

// buildLayer writes the Windows container layer holding arcane.exe. The format
// is not an ordinary rootfs tar: moby's Windows graphdriver reads Win32
// metadata back out of MSWINDOWS.* pax records and replays it through
// hcsshim's layer writer, so the metadata below is required, not cosmetic. A
// layer missing it fails to import with ERROR_PATH_NOT_FOUND.
func buildLayer(exePath string) (string, error) {
	st, err := os.Stat(exePath)
	if err != nil {
		return "", err
	}

	out, err := os.CreateTemp("", "arcane-win-layer-*.tar")
	if err != nil {
		return "", err
	}
	defer out.Close()

	tw := tar.NewWriter(out)
	now := time.Now()

	// Hives holds registry deltas. Every Windows layer carries the directory
	// even when it holds nothing, and the import fails without it.
	if err := writeDir(tw, "Hives", now, ""); err != nil {
		return "", err
	}
	if err := writeDir(tw, "Files", now, ""); err != nil {
		return "", err
	}
	if err := writeDir(tw, "Files/arcane", now, dirSecurityDescriptor); err != nil {
		return "", err
	}
	if err := writeDir(tw, "Files/arcane/data", now, dirSecurityDescriptor); err != nil {
		return "", err
	}

	hdr := &tar.Header{
		Format:   tar.FormatPAX,
		Name:     "Files/arcane/arcane.exe",
		Typeflag: tar.TypeReg,
		Mode:     0o755,
		Size:     st.Size(),
		ModTime:  now,
		PAXRecords: map[string]string{
			"MSWINDOWS.fileattr":      fmt.Sprint(fileAttributeArchive),
			"MSWINDOWS.rawsd":         fileSecurityDescriptor,
			"LIBARCHIVE.creationtime": fmt.Sprint(now.Unix()),
		},
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return "", err
	}
	exe, err := os.Open(exePath)
	if err != nil {
		return "", err
	}
	defer exe.Close()
	if _, err := io.Copy(tw, exe); err != nil {
		return "", err
	}
	if err := tw.Close(); err != nil {
		return "", err
	}

	fmt.Printf("layer: %s (%d bytes of exe)\n", filepath.Base(out.Name()), st.Size())
	return out.Name(), nil
}

// writeDir adds a directory entry. Real Windows layers name directories
// without a trailing slash and mark them with the directory file attribute.
func writeDir(tw *tar.Writer, name string, modTime time.Time, securityDescriptor string) error {
	pax := map[string]string{
		"MSWINDOWS.fileattr":      fmt.Sprint(fileAttributeDirectory),
		"LIBARCHIVE.creationtime": fmt.Sprint(modTime.Unix()),
	}
	if securityDescriptor != "" {
		pax["MSWINDOWS.rawsd"] = securityDescriptor
	}
	return tw.WriteHeader(&tar.Header{
		Format:     tar.FormatPAX,
		Name:       name,
		Typeflag:   tar.TypeDir,
		Mode:       0o755,
		ModTime:    modTime,
		PAXRecords: pax,
	})
}

// verify fails the build rather than shipping an image docker would reject at
// pull time.
func verify(img v1.Image) error {
	cfg, err := img.ConfigFile()
	if err != nil {
		return err
	}
	layers, err := img.Layers()
	if err != nil {
		return err
	}
	if len(layers) != len(cfg.RootFS.DiffIDs) {
		return fmt.Errorf("manifest has %d layers but config lists %d diff_ids", len(layers), len(cfg.RootFS.DiffIDs))
	}
	for i, l := range layers {
		diffID, err := l.DiffID()
		if err != nil {
			return err
		}
		if diffID != cfg.RootFS.DiffIDs[i] {
			return fmt.Errorf("layer %d diff_id %s does not match config %s", i, diffID, cfg.RootFS.DiffIDs[i])
		}
	}
	if cfg.OS != "windows" || cfg.OSVersion == "" {
		b, _ := json.Marshal(cfg.Platform())
		return fmt.Errorf("image does not look like a Windows image: %s", b)
	}
	fmt.Printf("verified: %d layers, diff_ids match, os=%s os.version=%s\n", len(layers), cfg.OS, cfg.OSVersion)
	return nil
}

func mustRef(s string) name.Reference {
	ref, err := name.ParseReference(s)
	must(err)
	return ref
}

func must(err error) {
	if err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
