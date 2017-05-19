package buildtree

import (
	"fmt"

	"github.com/docker/docker/api/types/strslice"
)

func (s *entryPointBuildStep) run(ctx *stageContext) error {
	switch {
	case s.disabled:
		ctx.runConfig.Entrypoint = nil
	case s.prependShell:
		ctx.runConfig.Entrypoint = strslice.StrSlice(append(getShell(ctx.runConfig), s.commandLine...))
	default:
		ctx.runConfig.Entrypoint = strslice.StrSlice(s.commandLine)
	}
	// when setting the entrypoint if a CMD was not explicitly set then
	// set the command to nil
	if !ctx.cmdSet {
		ctx.runConfig.Cmd = nil
	}

	return ctx.commit(fmt.Sprintf("ENTRYPOINT %q", ctx.runConfig.Entrypoint))
}
