package buildtree

func (s *onbuildBuildStep) run(ctx *stageContext) error {
	ctx.runConfig.OnBuild = append(ctx.runConfig.OnBuild, s.expression)
	return ctx.commit("ONBUILD " + s.expression)
}
