package buildtree

import (
	"errors"
	"fmt"
)

func (s *healthCheckBuildStep) run(ctx *stageContext) error {
	if s.healthCheck == nil {
		return errors.New("healthcheck is nil")
	}

	if ctx.runConfig.Healthcheck != nil && len(ctx.runConfig.Healthcheck.Test) > 0 && ctx.runConfig.Healthcheck.Test[0] != "NONE" {

		fmt.Fprintf(ctx.commonContext.stdout, "Note: overriding previous HEALTHCHECK: %v\n", ctx.runConfig.Healthcheck.Test)
	}
	ctx.runConfig.Healthcheck = s.healthCheck
	return ctx.commit(fmt.Sprintf("HEALTHCHECK %q", ctx.runConfig.Healthcheck))
}
