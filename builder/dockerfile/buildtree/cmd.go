package buildtree

import (
	"fmt"

	"github.com/docker/docker/api/types/strslice"
)

func (s *cmdBuildStep) run(ctx *stageContext) error {
	cmdLine := s.commandLine
	if s.prependShell {
		cmdLine = append(getShell(ctx.runConfig), cmdLine...)
	}
	ctx.runConfig.Cmd = strslice.StrSlice(cmdLine)
	ctx.runConfig.ArgsEscaped = true
	ctx.cmdSet = true
	return ctx.commit(fmt.Sprintf("CMD %q", cmdLine))
}
