package buildtree

func (s *argsBuildStep) run(ctx *stageContext) error {
	// arg is executed at build-tree generation time, but we commit a layer so that the same input w/wo the same args don't trigger a common cache
	comment := "ARG " + s.name
	if s.value != nil {
		comment = comment + "=" + *s.value
	}
	return ctx.commit(comment)
}
