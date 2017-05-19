package buildtree

import (
	"github.com/pkg/errors"

	"github.com/docker/docker/builder"
)

func (s *copyBuildStep) run(ctx *stageContext) error {
	var mount builder.ImageMount
	var err error
	if s.sourceStageOrImageRef != "" {
		if foreign, ok := ctx.commonContext.stagesContexts[s.sourceStageOrImageRef]; ok {
			<-foreign.done // wait foreign context is built
			if foreign.err != nil {
				return errors.Wrap(foreign.err, "foreign context has error")
			}
			mount, err = ctx.commonContext.imageSources.Get(foreign.result.ImageID)
			if err != nil {
				return err
			}
		} else {
			mount, err = ctx.commonContext.imageSources.Get(s.sourceStageOrImageRef)
			if err != nil {
				return err
			}
		}
	}
	copier := copierFromContext(ctx.commonContext, errOnSourceDownload, mount)
	defer copier.Cleanup()
	copyInstruction, err := copier.createCopyInstruction(append(s.sources, s.dest), "COPY")
	if err != nil {
		return err
	}

	return ctx.performCopy(copyInstruction)
}
