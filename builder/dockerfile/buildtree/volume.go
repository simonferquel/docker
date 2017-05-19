package buildtree

import "fmt"

func (s *volummesBuildStep) run(ctx *stageContext) error {
	for _, v := range s.volumes {
		ctx.runConfig.Volumes[v] = struct{}{}
	}
	return ctx.commit(fmt.Sprintf("VOLUME %v", s.volumes))
}
