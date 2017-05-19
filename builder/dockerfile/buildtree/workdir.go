package buildtree

import (
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
)

func (s *workdirBuildStep) run(ctx *stageContext) error {
	var err error
	ctx.runConfig.WorkingDir, err = normaliseWorkdir(ctx.runConfig.WorkingDir, s.wd)
	if err != nil {
		return err
	}
	if ctx.commonContext.disableCommit {
		return nil
	}

	comment := "WORKDIR " + ctx.runConfig.WorkingDir
	runConfigWithCommentCmd := copyRunConfig(ctx.runConfig, withCmdCommentString(comment))
	if hit, err := ctx.probeCache(runConfigWithCommentCmd); err != nil || hit {
		return err
	}

	container, err := ctx.commonContext.docker.ContainerCreate(types.ContainerCreateConfig{
		Config: runConfigWithCommentCmd,
		// Set a log config to override any default value set on the daemon
		HostConfig: &container.HostConfig{LogConfig: defaultLogConfig},
	})
	if err != nil {
		return err
	}
	ctx.tmpContainers[container.ID] = struct{}{}
	if err := ctx.commonContext.docker.ContainerCreateWorkdir(container.ID); err != nil {
		return err
	}

	return ctx.commitContainer(container.ID, runConfigWithCommentCmd)
}
