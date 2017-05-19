package buildtree

import (
	"strings"

	"github.com/docker/go-connections/nat"
)

func (s *exposeBuildStep) run(ctx *stageContext) error {
	runConfig := ctx.runConfig
	if runConfig.ExposedPorts == nil {
		runConfig.ExposedPorts = make(nat.PortSet)
	}

	for _, port := range s.exposedPorts {
		if _, exists := runConfig.ExposedPorts[nat.Port(port)]; !exists {
			runConfig.ExposedPorts[nat.Port(port)] = struct{}{}
		}
	}
	return ctx.commit("EXPOSE " + strings.Join(s.exposedPorts, " "))
}
