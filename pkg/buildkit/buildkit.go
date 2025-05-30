package buildkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/containerd/platforms"
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/client/llb/sourceresolver"
	gwclient "github.com/moby/buildkit/frontend/gateway/client"
	ispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/project-copacetic/copacetic/pkg/report"
	log "github.com/sirupsen/logrus"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

type Config struct {
	ImageName         string
	Client            gwclient.Client
	ConfigData        []byte
	PatchedConfigData []byte
	Platform          *ispec.Platform
	ImageState        llb.State
	PatchedImageState llb.State
}

type Opts struct {
	Addr       string
	CACertPath string
	CertPath   string
	KeyPath    string
}

const linux = "linux"

func InitializeBuildkitConfig(ctx context.Context, c gwclient.Client, userImage string) (*Config, error) {
	// Initialize buildkit config for the target image
	config := Config{
		ImageName: userImage,
	}

	// Resolve and pull the config for the target image
	_, _, configData, err := c.ResolveImageConfig(ctx, userImage, sourceresolver.Opt{
		ImageOpt: &sourceresolver.ResolveImageOpt{
			ResolveMode: llb.ResolveModePreferLocal.String(),
		},
	})
	if err != nil {
		return nil, err
	}

	var baseImage string
	config.ConfigData, config.PatchedConfigData, baseImage, err = updateImageConfigData(ctx, c, configData, userImage)
	if err != nil {
		return nil, err
	}

	// Load the target image state with the resolved image config in case environment variable settings
	// are necessary for running apps in the target image for updates
	config.ImageState, err = llb.Image(baseImage,
		llb.ResolveModePreferLocal,
		llb.WithMetaResolver(c),
	).WithImageConfig(config.ConfigData)
	if err != nil {
		return nil, err
	}

	// Only set PatchedImageState if the user supplied a patched image
	// An image is deemed to be a patched image if it contains one of two metadata values
	// BaseImage or ispec.AnnotationBaseImageName
	if config.PatchedConfigData != nil {
		config.PatchedImageState, err = llb.Image(userImage,
			llb.ResolveModePreferLocal,
			llb.WithMetaResolver(c),
		).WithImageConfig(config.PatchedConfigData)
		if err != nil {
			return nil, err
		}
	}

	config.Client = c

	return &config, nil
}

func DiscoverPlatformsFromReport(reportDir, scanner string) ([]ispec.Platform, error) {
	var platforms []ispec.Platform

	reportNames, err := os.ReadDir(reportDir)
	if err != nil {
		return nil, err
	}

	for _, file := range reportNames {
		report, err := report.TryParseScanReport(reportDir+"/"+file.Name(), scanner)
		if err != nil {
			return nil, fmt.Errorf("error parsing report %w", err)
		}

		// use this to confirm that os type (ex/Debian) is linux based and supported since report.Metadata.OS.Type gives specific like "debian" rather than "linux"
		if !isSupportedOsType(report.Metadata.OS.Type) {
			continue
		}

		platform := ispec.Platform{
			OS:           linux,
			Architecture: report.Metadata.Config.Arch,
		}
		platforms = append(platforms, platform)
	}

	return platforms, nil
}

func isSupportedOsType(osType string) bool {
	switch osType {
	case "alpine", "debian", "ubuntu", "cbl-mariner", "azurelinux", "centos", "oracle", "redhat", "rocky", "amazon", "alma":
		return true
	default:
		return false
	}
}

// This approach will not work for local images, add future support for this.
func DiscoverPlatformsFromReference(manifestRef string) ([]ispec.Platform, error) {
	var platforms []ispec.Platform

	ref, err := name.ParseReference(manifestRef)
	if err != nil {
		return nil, fmt.Errorf("error parsing reference %q: %w", manifestRef, err)
	}

	desc, err := remote.Get(ref)
	if err != nil {
		return nil, fmt.Errorf("error fetching descriptor for %q: %w", manifestRef, err)
	}

	if desc.MediaType.IsIndex() {
		index, err := desc.ImageIndex()
		if err != nil {
			return nil, fmt.Errorf("error getting image index %w", err)
		}

		manifest, err := index.IndexManifest()
		if err != nil {
			return nil, fmt.Errorf("error getting manifest: %w", err)
		}

		for i := range manifest.Manifests {
			m := &manifest.Manifests[i]

			if m.Platform.OS != linux {
				continue
			}
			platforms = append(platforms, ispec.Platform{
				OS:           m.Platform.OS,
				Architecture: m.Platform.Architecture,
			})
		}
		return platforms, nil
	}

	// return nil if not multi-arch, and handle as normal
	return nil, nil
}

func DiscoverPlatforms(manifestRef, reportDir, scanner string) ([]ispec.Platform, error) {
	var platforms []ispec.Platform

	p, err := DiscoverPlatformsFromReference(manifestRef)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, errors.New("image is not multi arch")
	}
	log.Debug("Discovered platforms from manifest:", p)

	if reportDir != "" {
		p2, err := DiscoverPlatformsFromReport(reportDir, scanner)
		if err != nil {
			return nil, err
		}
		log.Debug("Discovered platforms from report:", p2)

		// if platform is present in list from reference and report, then we should patch that platform
		key := func(pl ispec.Platform) string {
			return pl.OS + "/" + pl.Architecture
		}

		reportSet := make(map[string]struct{}, len(p2))
		for _, pl := range p2 {
			reportSet[key(pl)] = struct{}{}
		}

		for _, pl := range p {
			if _, ok := reportSet[key(pl)]; ok {
				platforms = append(platforms, pl)
			}
		}

		return platforms, nil
	}

	return p, nil
}

func updateImageConfigData(ctx context.Context, c gwclient.Client, configData []byte, image string) ([]byte, []byte, string, error) {
	baseImage, userImageConfig, err := setupLabels(image, configData)
	if err != nil {
		return nil, nil, "", err
	}

	if baseImage == "" {
		configData = userImageConfig
	} else {
		patchedImageConfig := userImageConfig
		_, _, baseImageConfig, err := c.ResolveImageConfig(ctx, baseImage, sourceresolver.Opt{
			ImageOpt: &sourceresolver.ResolveImageOpt{
				ResolveMode: llb.ResolveModePreferLocal.String(),
			},
		})
		if err != nil {
			return nil, nil, "", err
		}

		_, baseImageWithLabels, _ := setupLabels(baseImage, baseImageConfig)
		configData = baseImageWithLabels

		return configData, patchedImageConfig, baseImage, nil
	}

	return configData, nil, image, nil
}

func setupLabels(image string, configData []byte) (string, []byte, error) {
	imageConfig := make(map[string]interface{})
	err := json.Unmarshal(configData, &imageConfig)
	if err != nil {
		return "", nil, err
	}

	configMap, ok := imageConfig["config"].(map[string]interface{})
	if !ok {
		err := fmt.Errorf("type assertion to map[string]interface{} failed")
		return "", nil, err
	}

	var baseImage string
	labels := configMap["labels"]
	if labels == nil {
		configMap["labels"] = make(map[string]interface{})
	}
	labelsMap, ok := configMap["labels"].(map[string]interface{})
	if !ok {
		err := fmt.Errorf("type assertion to map[string]interface{} failed")
		return "", nil, err
	}
	if baseImageValue := labelsMap["BaseImage"]; baseImageValue != nil {
		baseImage, ok = baseImageValue.(string)
		if !ok {
			err := fmt.Errorf("type assertion to string failed")
			return "", nil, err
		}
	} else {
		labelsMap["BaseImage"] = image
	}

	imageWithLabels, _ := json.Marshal(imageConfig)

	return baseImage, imageWithLabels, nil
}

// Extracts the bytes of the file denoted by `path` from the state `st`.
func ExtractFileFromState(ctx context.Context, c gwclient.Client, st *llb.State, path string) ([]byte, error) {
	// since platform is obtained from host, override it in the case of Darwin
	platform := platforms.Normalize(platforms.DefaultSpec())
	if platform.OS != linux {
		platform.OS = linux
	}

	def, err := st.Marshal(ctx, llb.Platform(platform))
	if err != nil {
		return nil, err
	}

	resp, err := c.Solve(ctx, gwclient.SolveRequest{
		Evaluate:   true,
		Definition: def.ToPB(),
	})
	if err != nil {
		return nil, err
	}

	ref, err := resp.SingleRef()
	if err != nil {
		return nil, err
	}

	return ref.ReadFile(ctx, gwclient.ReadRequest{
		Filename: path,
	})
}

func Sh(cmd string) llb.RunOption {
	return llb.Args([]string{"/bin/sh", "-c", cmd})
}

func ArrayFile(input []string) []byte {
	var b bytes.Buffer
	for _, s := range input {
		b.WriteString(s)
		b.WriteRune('\n') // newline
	}
	return b.Bytes()
}

func WithArrayFile(s *llb.State, path string, contents []string) llb.State {
	af := ArrayFile(contents)
	return WithFileBytes(s, path, af)
}

func WithFileString(s *llb.State, path, contents string) llb.State {
	return WithFileBytes(s, path, []byte(contents))
}

func WithFileBytes(s *llb.State, path string, contents []byte) llb.State {
	return s.File(llb.Mkfile(path, 0o600, contents))
}
