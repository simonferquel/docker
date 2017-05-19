package buildtree

import (
	"sort"
	"strings"
)

func (s *labelsBuildStep) run(ctx *stageContext) error {
	sortedLabels := []string{} // we need to sort labels so that to create the same commit message
	if ctx.runConfig.Labels == nil {
		ctx.runConfig.Labels = map[string]string{}
	}

	for name, value := range s.labels {
		ctx.runConfig.Labels[name] = value
		sortedLabels = append(sortedLabels, name+"="+value)
	}
	sort.Strings(sortedLabels)
	return ctx.commit("LABEL " + strings.Join(sortedLabels, " "))
}
