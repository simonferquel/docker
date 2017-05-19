package buildtree

import (
	"fmt"

	"github.com/docker/docker/api/types/strslice"
)

func (s *shellBuildStep) run(ctx *stageContext) error {
	ctx.runConfig.Shell = strslice.StrSlice(s.shell)

	return ctx.commit(fmt.Sprintf("SHELL %v", s.shell))
}
