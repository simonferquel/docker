package buildtree

import (
	"context"
	"io"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/builder"
)

type buildContext struct {
	options *types.ImageBuildOptions

	ctx           context.Context
	disableCommit bool
	noCache       bool
	cacheBusted   bool
	imageCache    builder.ImageCache

	imageSources builder.ImageMounts
	pathCache    pathCache

	docker builder.Backend
	output io.Writer
	stdout io.Writer
	stderr io.Writer
	source builder.Source

	stagesContexts map[string]*stageContext
	buildArgs      *builder.BuildArgs
}

type pathCache interface {
	Load(key interface{}) (value interface{}, ok bool)
	Store(key, value interface{})
}

type stageContext struct {
	done      chan interface{} // channel indicating that the build for this stage is done. Used for cross-context synchronization (copy from etc.)
	imageID   string
	runConfig *container.Config
	source    builder.Source

	maintainer    string
	tmpContainers map[string]struct{}

	commonContext *buildContext

	result *builder.Result
	err    error

	cmdSet bool
}

func (s *stageContext) update(imageId string, runConfig *container.Config) {
	s.imageID = imageId
	s.runConfig = runConfig
}
