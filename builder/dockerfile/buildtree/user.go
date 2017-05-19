package buildtree

import "fmt"

func (s *userBuildStep) run(ctx *stageContext) error {

	ctx.runConfig.User = s.user
	return ctx.commit(fmt.Sprintf("USER %v", s.user))
}
