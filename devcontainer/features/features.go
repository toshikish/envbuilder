package features

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/go-git/go-billy/v5"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/otiai10/copy"
	"github.com/tailscale/hujson"
)

func extractFromImage(fs billy.Filesystem, directory, reference string) error {
	ref, err := name.ParseReference(reference)
	if err != nil {
		return fmt.Errorf("parse feature ref %s: %w", reference, err)
	}
	image, err := remote.Image(ref)
	if err != nil {
		return fmt.Errorf("fetch feature image %s: %w", reference, err)
	}
	manifest, err := image.Manifest()
	if err != nil {
		return fmt.Errorf("fetch feature manifest %s: %w", reference, err)
	}

	var tarLayer *tar.Reader
	for _, manifestLayer := range manifest.Layers {
		if manifestLayer.MediaType != TarLayerMediaType {
			continue
		}
		layer, err := image.LayerByDigest(manifestLayer.Digest)
		if err != nil {
			return fmt.Errorf("fetch feature layer %s: %w", reference, err)
		}
		layerReader, err := layer.Uncompressed()
		if err != nil {
			return fmt.Errorf("uncompress feature layer %s: %w", reference, err)
		}
		tarLayer = tar.NewReader(layerReader)
		break
	}
	if tarLayer == nil {
		return fmt.Errorf("no tar layer found with media type %q: are you sure this is a devcontainer feature?", TarLayerMediaType)
	}

	for {
		header, err := tarLayer.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read feature layer %s: %w", reference, err)
		}
		path := filepath.Join(directory, header.Name)
		switch header.Typeflag {
		case tar.TypeDir:
			err = fs.MkdirAll(path, 0755)
			if err != nil {
				return fmt.Errorf("mkdir %s: %w", path, err)
			}
		case tar.TypeReg:
			outFile, err := fs.Create(path)
			if err != nil {
				return fmt.Errorf("create %s: %w", path, err)
			}
			_, err = io.Copy(outFile, tarLayer)
			if err != nil {
				return fmt.Errorf("copy %s: %w", path, err)
			}
			err = outFile.Close()
			if err != nil {
				return fmt.Errorf("close %s: %w", path, err)
			}
		default:
			return fmt.Errorf("unknown type %d in %s", header.Typeflag, path)
		}
	}
	return nil
}

// Extract unpacks the feature from the image and returns the
// parsed specification.
func Extract(fs billy.Filesystem, devcontainerDir, directory, reference string) (*Spec, error) {
	if strings.HasPrefix(reference, "./") {
		if err := copy.Copy(filepath.Join(devcontainerDir, reference), directory, copy.Options{
			PreserveTimes: true,
			PreserveOwner: true,
			OnSymlink: func(src string) copy.SymlinkAction {
				return copy.Shallow
			},
			OnError: func(src, dest string, err error) error {
				if err == nil {
					return nil
				}
				return fmt.Errorf("copy error: %q -> %q: %w", reference, directory, err)
			},
		}); err != nil {
			return nil, err
		}
	} else if err := extractFromImage(fs, directory, reference); err != nil {
		return nil, err
	}

	installScriptPath := filepath.Join(directory, "install.sh")
	_, err := fs.Stat(installScriptPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("install.sh must be in the root of the feature")
		}
		return nil, fmt.Errorf("stat install.sh: %w", err)
	}
	chmodder, ok := fs.(interface {
		Chmod(name string, mode os.FileMode) error
	})
	if ok {
		// For some reason the filesystem abstraction doesn't support chmod.
		// https://github.com/src-d/go-billy/issues/56
		err = chmodder.Chmod(installScriptPath, 0755)
	}
	if err != nil {
		return nil, fmt.Errorf("chmod install.sh: %w", err)
	}
	featureFile, err := fs.Open(filepath.Join(directory, "devcontainer-feature.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("devcontainer-feature.json must be in the root of the feature")
		}
		return nil, fmt.Errorf("open devcontainer-feature.json: %w", err)
	}
	defer featureFile.Close()
	featureFileBytes, err := io.ReadAll(featureFile)
	if err != nil {
		return nil, fmt.Errorf("read devcontainer-feature.json: %w", err)
	}
	standardizedFeatureFileBytes, err := hujson.Standardize(featureFileBytes)
	if err != nil {
		return nil, fmt.Errorf("standardize devcontainer-feature.json: %w", err)
	}
	var spec *Spec
	if err := json.Unmarshal(standardizedFeatureFileBytes, &spec); err != nil {
		return nil, fmt.Errorf("decode devcontainer-feature.json: %w", err)
	}
	// See https://containers.dev/implementors/features/#devcontainer-feature-json-properties
	if spec.ID == "" {
		return nil, errors.New(`devcontainer-feature.json: id is required`)
	}
	if spec.Version == "" {
		return nil, errors.New(`devcontainer-feature.json: version is required`)
	}
	if spec.Name == "" {
		return nil, errors.New(`devcontainer-feature.json: name is required`)
	}

	spec.Directory = directory
	return spec, nil
}

const (
	TarLayerMediaType = "application/vnd.devcontainers.layer.v1+tar"
)

type Option struct {
	Type        string   `json:"type"` // "boolean" or "string"
	Proposals   []string `json:"proposals"`
	Enum        []string `json:"enum"`
	Default     any      `json:"default"` // boolean or string
	Description string   `json:"description"`
}

type Spec struct {
	ID               string            `json:"id"`
	Version          string            `json:"version"`
	Name             string            `json:"name"`
	Description      string            `json:"description"`
	DocumentationURL string            `json:"documentationURL"`
	LicenseURL       string            `json:"licenseURL"`
	Keywords         []string          `json:"keywords"`
	Options          map[string]Option `json:"options"`
	ContainerEnv     map[string]string `json:"containerEnv"`

	Directory string `json:"-"`
}

// Extract unpacks the feature from the image and returns a set of lines
// that should be appended to a Dockerfile to install the feature.
func (s *Spec) Compile(featureName, containerUser, remoteUser string, useBuildContexts bool, options map[string]any) (string, string, error) {
	// TODO not sure how we figure out _(REMOTE|CONTAINER)_USER_HOME
	// as per the feature spec.
	// See https://containers.dev/implementors/features/#user-env-var
	var fromDirective string
	runDirective := []string{
		"_CONTAINER_USER=" + strconv.Quote(containerUser),
		"_REMOTE_USER=" + strconv.Quote(remoteUser),
	}
	for key, value := range s.Options {
		strValue := fmt.Sprint(value.Default)
		provided, ok := options[key]
		if ok {
			strValue = fmt.Sprint(provided)
			// delete so we can check if there are any unknown options
			delete(options, key)
		}
		runDirective = append(runDirective, fmt.Sprintf(`%s=%q`, convertOptionNameToEnv(key), strValue))
	}
	if len(options) > 0 {
		return "", "", fmt.Errorf("unknown option: %v", options)
	}
	// It's critical that the Dockerfile produced is deterministic,
	// regardless of map iteration order.
	sort.Strings(runDirective)
	// See https://containers.dev/implementors/features/#invoking-installsh
	if useBuildContexts {
		fromDirective = "FROM scratch AS envbuilder_feature_" + featureName + "\nCOPY --from=" + featureName + " / /\n"
		runDirective = append([]string{"RUN", "--mount=type=bind,from=envbuilder_feature_" + featureName + ",target=/envbuilder-features/" + featureName + ",rw"}, runDirective...)
	} else {
		runDirective = append([]string{"RUN"}, runDirective...)
	}
	runDirective = append(runDirective, "./install.sh")

	comment := ""
	if s.Name != "" {
		comment += "# " + s.Name
	}
	if s.Version != "" {
		comment += " " + s.Version
	}
	if s.Description != "" {
		comment += " - " + s.Description
	}
	lines := []string{}
	if comment != "" {
		lines = append(lines, comment)
	}
	if useBuildContexts {
		lines = append(lines, "WORKDIR /envbuilder-features/"+featureName)
	} else {
		lines = append(lines, "WORKDIR "+s.Directory)
	}
	envKeys := make([]string, 0, len(s.ContainerEnv))
	for key := range s.ContainerEnv {
		envKeys = append(envKeys, key)
	}
	// It's critical that the Dockerfile produced is deterministic,
	// regardless of map iteration order.
	sort.Strings(envKeys)
	for _, key := range envKeys {
		lines = append(lines, fmt.Sprintf("ENV %s=%s", key, s.ContainerEnv[key]))
	}
	lines = append(lines, strings.Join(runDirective, " "))

	return fromDirective, strings.Join(lines, "\n"), nil
}

var (
	matchNonWords                   = regexp.MustCompile(`/[^\w_]/g`)
	matchPrefixDigitsAndUnderscores = regexp.MustCompile(`/^[\d_]+/g`)
)

// See https://containers.dev/implementors/features/#option-resolution
func convertOptionNameToEnv(optionName string) string {
	optionName = matchNonWords.ReplaceAllString(optionName, "_")
	optionName = matchPrefixDigitsAndUnderscores.ReplaceAllString(optionName, "")
	return strings.ToUpper(optionName)
}
