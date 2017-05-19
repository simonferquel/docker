package buildtree

import "fmt"

func (s *stopsignalBuildStep) run(ctx *stageContext) error {

	ctx.runConfig.StopSignal = s.signal
	return ctx.commit(fmt.Sprintf("STOPSIGNAL %v", s.signal))
}
