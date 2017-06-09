package dockerfile

import (
	"strconv"
	"strings"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/api/types/backend"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/builder"
	"github.com/docker/docker/builder/remotecontext"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
)

type buildStage struct {
	id     string
	config *container.Config
	done   chan struct{}
	failed chan struct{}
}

func newBuildStage() *buildStage {
	return &buildStage{done: make(chan struct{}), failed: make(chan struct{})}
}

func (b *buildStage) Init(image builder.Image) {
	b.id = image.ImageID()
	b.config = image.RunConfig()
}

func (b *buildStage) ImageID() string {
	return b.id
}

func (b *buildStage) RunConfig() *container.Config {
	return b.config
}

func (b *buildStage) update(imageID string, runConfig *container.Config) {
	b.id = imageID
	b.config = runConfig
}

func (b *buildStage) complete() {
	close(b.done)
}
func (b *buildStage) fail() {
	close(b.failed)
}

func (b *buildStage) wait(cancelCtx context.Context) error {
	select {
	case <-b.done:
		return nil
	case <-b.failed:
		return errors.New("Parent stage failed")
	case <-cancelCtx.Done():
		return errors.New("Build canceled")
	}
}

var _ builder.Image = &buildStage{}

// buildStages tracks each stage of a build so they can be retrieved by index
// or by name.
type buildStages struct {
	sequence []*buildStage
	byName   map[string]*buildStage
}

func newBuildStages() *buildStages {
	return &buildStages{byName: make(map[string]*buildStage)}
}

func (s *buildStages) getByName(cancelCtx context.Context, name string) (builder.Image, bool) {
	stage, ok := s.byName[strings.ToLower(name)]
	if ok {
		stage.wait(cancelCtx)
	}
	return stage, ok
}

func (s *buildStages) get(cancelCtx context.Context, indexOrName string) (builder.Image, error) {
	index, err := strconv.Atoi(indexOrName)
	if err == nil {
		if err := s.validateIndex(index); err != nil {
			return nil, err
		}
		stage := s.sequence[index]
		stage.wait(cancelCtx)
		return stage, nil
	}
	if im, ok := s.getByName(cancelCtx, indexOrName); ok {
		return im, nil
	}
	return nil, nil
}

func (s *buildStages) validateIndex(i int) error {
	if i < 0 || i >= len(s.sequence)-1 {
		if i == len(s.sequence)-1 {
			return errors.New("refers to current build stage")
		}
		return errors.New("index out of bounds")
	}
	return nil
}

func (s *buildStages) add(name string) (*buildStage, error) {
	stage := newBuildStage()
	name = strings.ToLower(name)
	if len(name) > 0 {
		if _, ok := s.byName[name]; ok {
			return nil, errors.Errorf("duplicate name %s", name)
		}
		s.byName[name] = stage
	}
	s.sequence = append(s.sequence, stage)
	return stage, nil
}

type getAndMountFunc func(string) (builder.Image, builder.ReleaseableLayer, error)

// imageSources mounts images and provides a cache for mounted images. It tracks
// all images so they can be unmounted at the end of the build.
type imageSources struct {
	byImageID map[string]*imageMount
	getImage  getAndMountFunc
	cache     pathCache // TODO: remove
}

func newImageSources(ctx context.Context, options builderOptions) *imageSources {
	getAndMount := func(idOrRef string) (builder.Image, builder.ReleaseableLayer, error) {
		return options.Backend.GetImageAndReleasableLayer(ctx, idOrRef, backend.GetImageAndLayerOptions{
			ForcePull:  options.Options.PullParent,
			AuthConfig: options.Options.AuthConfigs,
			Output:     options.ProgressWriter.Output,
		})
	}

	return &imageSources{
		byImageID: make(map[string]*imageMount),
		getImage:  getAndMount,
	}
}

func (m *imageSources) Get(idOrRef string) (*imageMount, error) {
	if im, ok := m.byImageID[idOrRef]; ok {
		return im, nil
	}

	image, layer, err := m.getImage(idOrRef)
	if err != nil {
		return nil, err
	}
	im := newImageMount(image, layer)
	m.byImageID[image.ImageID()] = im
	return im, nil
}

func (m *imageSources) Unmount() (retErr error) {
	for _, im := range m.byImageID {
		if err := im.unmount(); err != nil {
			logrus.Error(err)
			retErr = err
		}
	}
	return
}

// imageMount is a reference to an image that can be used as a builder.Source
type imageMount struct {
	image  builder.Image
	source builder.Source
	layer  builder.ReleaseableLayer
}

func newImageMount(image builder.Image, layer builder.ReleaseableLayer) *imageMount {
	im := &imageMount{image: image, layer: layer}
	return im
}

func (im *imageMount) Source() (builder.Source, error) {
	if im.source == nil {
		if im.layer == nil {
			return nil, errors.Errorf("empty context")
		}
		mountPath, err := im.layer.Mount()
		if err != nil {
			return nil, errors.Wrapf(err, "failed to mount %s", im.image.ImageID())
		}
		source, err := remotecontext.NewLazyContext(mountPath)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to create lazycontext for %s", mountPath)
		}
		im.source = source
	}
	return im.source, nil
}

func (im *imageMount) unmount() error {
	if im.layer == nil {
		return nil
	}
	if err := im.layer.Release(); err != nil {
		return errors.Wrapf(err, "failed to unmount previous build image %s", im.image.ImageID())
	}
	return nil
}

func (im *imageMount) Image() builder.Image {
	return im.image
}

func (im *imageMount) ImageID() string {
	return im.image.ImageID()
}
