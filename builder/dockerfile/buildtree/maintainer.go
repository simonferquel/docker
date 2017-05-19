package buildtree

func (m *maintainerBuildStep) run(ctx *stageContext) error {
	ctx.maintainer = m.name
	return ctx.commit("MAINTAINER " + m.name)
}
