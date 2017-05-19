package buildtree

func (e *envBuildStep) run(ctx *stageContext) error {
	ctx.runConfig.Env = e.envAfter
	return ctx.commit(e.commitMessage)
}
