package tarexport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"

	"github.com/containerd/containerd/v2/pkg/tracing"
	"github.com/containerd/log"
	"github.com/distribution/reference"
	"github.com/docker/distribution"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/image"
	v1 "github.com/docker/docker/image/v1"
	"github.com/docker/docker/internal/ioutils"
	"github.com/docker/docker/layer"
	"github.com/docker/docker/pkg/progress"
	"github.com/docker/docker/pkg/streamformatter"
	"github.com/docker/docker/pkg/stringid"
	"github.com/moby/go-archive/chrootarchive"
	"github.com/moby/go-archive/compression"
	"github.com/moby/sys/sequential"
	"github.com/moby/sys/symlink"
	"github.com/opencontainers/go-digest"
)

func (l *tarexporter) Load(ctx context.Context, inTar io.ReadCloser, outStream io.Writer, quiet bool) (outErr error) {
	ctx, span := tracing.StartSpan(ctx, "tarexport.Load")
	defer span.End()
	defer func() {
		span.SetStatus(outErr)
	}()

	var progressOutput progress.Output
	if !quiet {
		progressOutput = streamformatter.NewJSONProgressOutput(outStream, false)
	}
	outStream = streamformatter.NewStdoutWriter(outStream)

	tmpDir, err := os.MkdirTemp("", "docker-import-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	if err := untar(ctx, inTar, tmpDir); err != nil {
		return err
	}

	// read manifest, if no file then load in legacy mode
	manifestPath, err := safePath(tmpDir, manifestFileName)
	if err != nil {
		return err
	}
	manifestFile, err := os.Open(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return l.legacyLoad(tmpDir, outStream, progressOutput)
		}
		return err
	}
	defer manifestFile.Close()

	var manifest []manifestItem
	if err := json.NewDecoder(manifestFile).Decode(&manifest); err != nil {
		return err
	}

	if err := validateManifest(manifest); err != nil {
		return err
	}

	var parentLinks []parentLink
	var imageIDsStr string
	var imageRefCount int

	for _, m := range manifest {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		configPath, err := safePath(tmpDir, m.Config)
		if err != nil {
			return err
		}
		config, err := os.ReadFile(configPath)
		if err != nil {
			return err
		}
		img, err := image.NewFromJSON(config)
		if err != nil {
			return err
		}
		if err := image.CheckOS(img.OperatingSystem()); err != nil {
			return fmt.Errorf("cannot load %s image on %s", img.OperatingSystem(), runtime.GOOS)
		}
		if l.platformMatcher != nil && !l.platformMatcher.Match(img.Platform()) {
			continue
		}
		rootFS := *img.RootFS
		rootFS.DiffIDs = nil

		if expected, actual := len(m.Layers), len(img.RootFS.DiffIDs); expected != actual {
			return fmt.Errorf("invalid manifest, layers length mismatch: expected %d, got %d", expected, actual)
		}

		for i, diffID := range img.RootFS.DiffIDs {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			layerPath, err := safePath(tmpDir, m.Layers[i])
			if err != nil {
				return err
			}
			r := rootFS
			r.Append(diffID)
			newLayer, err := l.lss.Get(r.ChainID())
			if err != nil {
				newLayer, err = l.loadLayer(ctx, layerPath, rootFS, diffID.String(), m.LayerSources[diffID], progressOutput)
				if err != nil {
					return err
				}
			}
			defer layer.ReleaseAndLog(l.lss, newLayer)
			if expected, actual := diffID, newLayer.DiffID(); expected != actual {
				return fmt.Errorf("invalid diffID for layer %d: expected %q, got %q", i, expected, actual)
			}
			rootFS.Append(diffID)
		}

		imgID, err := l.is.Create(config)
		if err != nil {
			return err
		}
		imageIDsStr += fmt.Sprintf("Loaded image ID: %s\n", imgID)

		imageRefCount = 0
		for _, repoTag := range m.RepoTags {
			named, err := reference.ParseNormalizedNamed(repoTag)
			if err != nil {
				return err
			}
			ref, ok := named.(reference.NamedTagged)
			if !ok {
				return fmt.Errorf("invalid tag %q", repoTag)
			}
			l.setLoadedTag(ref, imgID.Digest(), outStream)
			fmt.Fprintf(outStream, "Loaded image: %s\n", reference.FamiliarString(ref))
			imageRefCount++
		}

		parentLinks = append(parentLinks, parentLink{imgID, m.Parent})
		l.loggerImgEvent.LogImageEvent(ctx, imgID.String(), imgID.String(), events.ActionLoad)
	}

	for _, p := range validatedParentLinks(parentLinks) {
		if p.parentID != "" {
			if err := l.setParentID(p.id, p.parentID); err != nil {
				return err
			}
		}
	}

	if imageRefCount == 0 {
		outStream.Write([]byte(imageIDsStr))
	}

	return nil
}

func untar(ctx context.Context, inTar io.ReadCloser, tmpDir string) error {
	_, trace := tracing.StartSpan(ctx, "chrootarchive.Untar")
	defer trace.End()

	err := chrootarchive.Untar(ioutils.NewCtxReader(ctx, inTar), tmpDir, nil)
	trace.SetStatus(err)
	return err
}

func (l *tarexporter) setParentID(id, parentID image.ID) error {
	img, err := l.is.Get(id)
	if err != nil {
		return err
	}
	parent, err := l.is.Get(parentID)
	if err != nil {
		return err
	}
	if !checkValidParent(img, parent) {
		return fmt.Errorf("image %v is not a valid parent for %v", parent.ID(), img.ID())
	}
	return l.is.SetParent(id, parentID)
}

func (l *tarexporter) loadLayer(ctx context.Context, filename string, rootFS image.RootFS, id string, foreignSrc distribution.Descriptor, progressOutput progress.Output) (_ layer.Layer, outErr error) {
	ctx, span := tracing.StartSpan(ctx, "loadLayer")
	span.SetAttributes(tracing.Attribute("image.id", id))
	defer span.End()
	defer func() {
		span.SetStatus(outErr)
	}()

	// We use sequential file access to avoid depleting the standby list on Windows.
	// On Linux, this equates to a regular os.Open.
	rawTar, err := sequential.Open(filename)
	if err != nil {
		log.G(context.TODO()).Debugf("Error reading embedded tar: %v", err)
		return nil, err
	}
	defer rawTar.Close()

	var r io.Reader
	if progressOutput != nil {
		fileInfo, err := rawTar.Stat()
		if err != nil {
			log.G(context.TODO()).Debugf("Error statting file: %v", err)
			return nil, err
		}

		r = progress.NewProgressReader(rawTar, progressOutput, fileInfo.Size(), stringid.TruncateID(id), "Loading layer")
	} else {
		r = rawTar
	}

	inflatedLayerData, err := compression.DecompressStream(ioutils.NewCtxReader(ctx, r))
	if err != nil {
		return nil, err
	}
	defer inflatedLayerData.Close()

	if ds, ok := l.lss.(layer.DescribableStore); ok {
		return ds.RegisterWithDescriptor(inflatedLayerData, rootFS.ChainID(), foreignSrc)
	}
	return l.lss.Register(inflatedLayerData, rootFS.ChainID())
}

func (l *tarexporter) setLoadedTag(ref reference.Named, imgID digest.Digest, outStream io.Writer) error {
	if prevID, err := l.rs.Get(ref); err == nil && prevID != imgID {
		fmt.Fprintf(outStream, "The image %s already exists, renaming the old one with ID %s to empty string\n", reference.FamiliarString(ref), string(prevID)) // todo: this message is wrong in case of multiple tags
	}

	return l.rs.AddTag(ref, imgID, true)
}

func (l *tarexporter) legacyLoad(tmpDir string, outStream io.Writer, progressOutput progress.Output) error {
	if runtime.GOOS == "windows" {
		return errors.New("Windows does not support legacy loading of images")
	}

	legacyLoadedMap := make(map[string]image.ID)

	dirs, err := os.ReadDir(tmpDir)
	if err != nil {
		return err
	}

	// every dir represents an image
	for _, d := range dirs {
		if d.IsDir() {
			if err := l.legacyLoadImage(d.Name(), tmpDir, legacyLoadedMap, progressOutput); err != nil {
				return err
			}
		}
	}

	// load tags from repositories file
	repositoriesPath, err := safePath(tmpDir, legacyRepositoriesFileName)
	if err != nil {
		return err
	}
	repositoriesFile, err := os.Open(repositoriesPath)
	if err != nil {
		return err
	}
	defer repositoriesFile.Close()

	repositories := make(map[string]map[string]string)
	if err := json.NewDecoder(repositoriesFile).Decode(&repositories); err != nil {
		return err
	}

	for name, tagMap := range repositories {
		for tag, oldID := range tagMap {
			imgID, ok := legacyLoadedMap[oldID]
			if !ok {
				return fmt.Errorf("invalid target ID: %v", oldID)
			}
			named, err := reference.ParseNormalizedNamed(name)
			if err != nil {
				return err
			}
			ref, err := reference.WithTag(named, tag)
			if err != nil {
				return err
			}
			l.setLoadedTag(ref, imgID.Digest(), outStream)
		}
	}

	return nil
}

func (l *tarexporter) legacyLoadImage(oldID, sourceDir string, loadedMap map[string]image.ID, progressOutput progress.Output) error {
	if _, loaded := loadedMap[oldID]; loaded {
		return nil
	}
	configPath, err := safePath(sourceDir, filepath.Join(oldID, legacyConfigFileName))
	if err != nil {
		return err
	}
	imageJSON, err := os.ReadFile(configPath)
	if err != nil {
		log.G(context.TODO()).Debugf("Error reading json: %v", err)
		return err
	}

	var img struct {
		OS     string
		Parent string
	}
	if err := json.Unmarshal(imageJSON, &img); err != nil {
		return err
	}

	if img.OS == "" {
		img.OS = runtime.GOOS
	}
	if err := image.CheckOS(img.OS); err != nil {
		return fmt.Errorf("cannot load %s image on %s", img.OS, runtime.GOOS)
	}

	var parentID image.ID
	if img.Parent != "" {
		for {
			var loaded bool
			if parentID, loaded = loadedMap[img.Parent]; !loaded {
				if err := l.legacyLoadImage(img.Parent, sourceDir, loadedMap, progressOutput); err != nil {
					return err
				}
			} else {
				break
			}
		}
	}

	// todo: try to connect with migrate code
	rootFS := image.NewRootFS()
	var history []image.History

	if parentID != "" {
		parentImg, err := l.is.Get(parentID)
		if err != nil {
			return err
		}

		rootFS = parentImg.RootFS
		history = parentImg.History
	}

	layerPath, err := safePath(sourceDir, filepath.Join(oldID, legacyLayerFileName))
	if err != nil {
		return err
	}
	newLayer, err := l.loadLayer(context.TODO(), layerPath, *rootFS, oldID, distribution.Descriptor{}, progressOutput)
	if err != nil {
		return err
	}
	rootFS.Append(newLayer.DiffID())

	h, err := v1.HistoryFromConfig(imageJSON, false)
	if err != nil {
		return err
	}
	history = append(history, h)

	config, err := v1.MakeConfigFromV1Config(imageJSON, rootFS, history)
	if err != nil {
		return err
	}
	imgID, err := l.is.Create(config)
	if err != nil {
		return err
	}

	metadata, err := l.lss.Release(newLayer)
	layer.LogReleaseMetadata(metadata)
	if err != nil {
		return err
	}

	if parentID != "" {
		if err := l.is.SetParent(imgID, parentID); err != nil {
			return err
		}
	}

	loadedMap[oldID] = imgID
	return nil
}

func safePath(base, path string) (string, error) {
	return symlink.FollowSymlinkInScope(filepath.Join(base, path), base)
}

type parentLink struct {
	id, parentID image.ID
}

func validatedParentLinks(pl []parentLink) (ret []parentLink) {
mainloop:
	for i, p := range pl {
		ret = append(ret, p)
		for _, p2 := range pl {
			if p2.id == p.parentID && p2.id != p.id {
				continue mainloop
			}
		}
		ret[i].parentID = ""
	}
	return ret
}

func checkValidParent(img, parent *image.Image) bool {
	if len(img.History) == 0 && len(parent.History) == 0 {
		return true // having history is not mandatory
	}
	if len(img.History)-len(parent.History) != 1 {
		return false
	}
	for i, hP := range parent.History {
		hC := img.History[i]
		if (hP.Created == nil) != (hC.Created == nil) {
			return false
		}
		if hP.Created != nil && !hP.Created.Equal(*hC.Created) {
			return false
		}
		hC.Created = hP.Created
		if !reflect.DeepEqual(hP, hC) {
			return false
		}
	}
	return true
}

func validateManifest(manifest []manifestItem) error {
	// a nil manifest usually indicates a bug, so don't just silently fail.
	// if someone really needs to pass an empty manifest, they can pass [].
	if manifest == nil {
		return errors.New("invalid manifest, manifest cannot be null (but can be [])")
	}

	return nil
}
