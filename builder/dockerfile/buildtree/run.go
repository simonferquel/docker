package buildtree

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/builder"
)

func prependEnvOnCmd(buildArgs *builder.BuildArgs, buildArgVars []string, cmd strslice.StrSlice) strslice.StrSlice {
	var tmpBuildEnv []string
	for _, env := range buildArgVars {
		key := strings.SplitN(env, "=", 2)[0]
		if buildArgs.IsReferencedOrNotBuiltin(key) {
			tmpBuildEnv = append(tmpBuildEnv, env)
		}
	}

	sort.Strings(tmpBuildEnv)
	tmpEnv := append([]string{fmt.Sprintf("|%d", len(tmpBuildEnv))}, tmpBuildEnv...)
	return strslice.StrSlice(append(tmpEnv, cmd...))
}

func (s *runBuildStep) run(ctx *stageContext) error {
	cmdLine := s.commandLine
	if s.prependShell {
		cmdLine = append(getShell(ctx.runConfig), cmdLine...)
	}

	stateRunConfig := ctx.runConfig
	cmdFromArgs := strslice.StrSlice(cmdLine)
	buildArgs := ctx.commonContext.buildArgs.FilterAllowed(stateRunConfig.Env)

	saveCmd := cmdFromArgs
	if len(buildArgs) > 0 {
		saveCmd = prependEnvOnCmd(ctx.commonContext.buildArgs, buildArgs, cmdFromArgs)
	}

	runConfigForCacheProbe := copyRunConfig(stateRunConfig,
		withCmd(saveCmd),
		withEntrypointOverride(saveCmd, nil))
	hit, err := ctx.probeCache(runConfigForCacheProbe)
	if err != nil || hit {
		return err
	}

	runConfig := copyRunConfig(stateRunConfig,
		withCmd(cmdFromArgs),
		withEnv(append(stateRunConfig.Env, buildArgs...)),
		withEntrypointOverride(saveCmd, strslice.StrSlice{""}))

	// set config as already being escaped, this prevents double escaping on windows
	runConfig.ArgsEscaped = true

	logrus.Debugf("[BUILDER] Command to be executed: %v", runConfig.Cmd)
	cID, err := ctx.create(runConfig)
	if err != nil {
		return err
	}
	if err := ctx.run(cID, runConfig.Cmd); err != nil {
		return err
	}

	return ctx.commitContainer(cID, runConfigForCacheProbe)
}
