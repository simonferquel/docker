package dockerfile

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"strings"

	"sync"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/backend"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/builder"
	"github.com/docker/docker/builder/dockerfile/instructions"
	"github.com/docker/docker/builder/dockerfile/parser"
	"github.com/docker/docker/builder/remotecontext"
	"github.com/docker/docker/pkg/streamformatter"
	"github.com/docker/docker/pkg/stringid"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
	"golang.org/x/sync/syncmap"
)

var validCommitCommands = map[string]bool{
	"cmd":         true,
	"entrypoint":  true,
	"healthcheck": true,
	"env":         true,
	"expose":      true,
	"label":       true,
	"onbuild":     true,
	"user":        true,
	"volume":      true,
	"workdir":     true,
}

const (
	stepFormat = "Step %d/%d : %v"
)

// BuildManager is shared across all Builder objects
type BuildManager struct {
	backend   builder.Backend
	pathCache pathCache // TODO: make this persistent
}

// NewBuildManager creates a BuildManager
func NewBuildManager(b builder.Backend) *BuildManager {
	return &BuildManager{
		backend:   b,
		pathCache: &syncmap.Map{},
	}
}

// Build starts a new build from a BuildConfig
func (bm *BuildManager) Build(ctx context.Context, config backend.BuildConfig) (*builder.Result, error) {
	buildsTriggered.Inc()
	if config.Options.Dockerfile == "" {
		config.Options.Dockerfile = builder.DefaultDockerfileName
	}

	source, dockerfile, err := remotecontext.Detect(config)
	if err != nil {
		return nil, err
	}
	if source != nil {
		defer func() {
			if err := source.Close(); err != nil {
				logrus.Debugf("[BUILDER] failed to remove temporary context: %v", err)
			}
		}()
	}

	builderOptions := builderOptions{
		Options:        config.Options,
		ProgressWriter: config.ProgressWriter,
		Backend:        bm.backend,
		PathCache:      bm.pathCache,
	}
	return newBuilder(ctx, builderOptions).build(source, dockerfile)
}

// builderOptions are the dependencies required by the builder
type builderOptions struct {
	Options        *types.ImageBuildOptions
	Backend        builder.Backend
	ProgressWriter backend.ProgressWriter
	PathCache      pathCache
}

// Builder is a Dockerfile builder
// It implements the builder.Backend interface.
type Builder struct {
	options *types.ImageBuildOptions

	Stdout io.Writer
	Stderr io.Writer
	Aux    *streamformatter.AuxFormatter
	Output io.Writer

	docker    builder.Backend
	clientCtx context.Context

	buildStages      *buildStages
	disableCommit    bool
	buildArgs        *buildArgs
	imageSources     *imageSources
	pathCache        pathCache
	containerManager *containerManager
	imageProber      ImageProber
}

// newBuilder creates a new Dockerfile builder from an optional dockerfile and a Options.
func newBuilder(clientCtx context.Context, options builderOptions) *Builder {
	config := options.Options
	if config == nil {
		config = new(types.ImageBuildOptions)
	}
	b := &Builder{
		clientCtx:        clientCtx,
		options:          config,
		Stdout:           options.ProgressWriter.StdoutFormatter,
		Stderr:           options.ProgressWriter.StderrFormatter,
		Aux:              options.ProgressWriter.AuxFormatter,
		Output:           options.ProgressWriter.Output,
		docker:           options.Backend,
		buildArgs:        newBuildArgs(config.BuildArgs),
		buildStages:      newBuildStages(),
		imageSources:     newImageSources(clientCtx, options),
		pathCache:        options.PathCache,
		imageProber:      newImageProber(options.Backend, config.CacheFrom, config.NoCache),
		containerManager: newContainerManager(options.Backend),
	}
	return b
}

// Build runs the Dockerfile builder by parsing the Dockerfile and executing
// the instructions from the file.
func (b *Builder) build(source builder.Source, dockerfile *parser.Result) (*builder.Result, error) {
	defer b.imageSources.Unmount()

	addNodesForLabelOption(dockerfile.AST, b.options.Labels)

	stages, metaArgs, err := instructions.Parse(dockerfile.AST)
	if err != nil {
		if instructions.IsUnknownInstruction(err) {
			buildsFailed.WithValues(metricsUnknownInstructionError).Inc()
		}
		return nil, err
	}
	if b.options.Target != "" {
		found, targetIx := instructions.HasStage(stages, b.options.Target)
		if !found {
			buildsFailed.WithValues(metricsBuildTargetNotReachableError).Inc()
			return nil, errors.Errorf("failed to reach build target %s in Dockerfile", b.options.Target)
		}
		stages = stages[:targetIx+1]
	}

	b.buildArgs.WarnOnUnusedBuildArgs(b.Stderr)

	dispatchState, err := b.dispatchDockerfileWithCancellation(stages, metaArgs, dockerfile.EscapeToken, source)
	if err != nil {
		return nil, err
	}
	if dispatchState.imageID == "" {
		buildsFailed.WithValues(metricsDockerfileEmptyError).Inc()
		return nil, errors.New("No image was generated. Is your Dockerfile empty?")
	}
	return &builder.Result{ImageID: dispatchState.imageID, FromImage: dispatchState.baseImage}, nil
}

func emitImageID(aux *streamformatter.AuxFormatter, state *dispatchState) error {
	if aux == nil || state.imageID == "" {
		return nil
	}
	return aux.Emit(types.BuildResult{ID: state.imageID})
}
func convertMapToEnvs(m map[string]string) []string {
	result := []string{}
	for k, v := range m {
		result = append(result, k+"="+v)
	}
	return result
}
func (b *Builder) processMetaArg(meta instructions.ArgCommand, shlex *ShellLex) error {
	envs := convertMapToEnvs(b.buildArgs.GetAllAllowed())
	if err := meta.Expand(func(word string) (string, error) {
		return shlex.ProcessWord(word, envs)
	}); err != nil {
		return err
	}
	b.buildArgs.AddArg(meta.Name, meta.Value)
	b.buildArgs.AddMetaArg(meta.Name, meta.Value)
	return nil
}

type stageBuilderAndCommands struct {
	builder  *stageBuilder
	commands []interface{}
	err      error
}

func (b *Builder) dispatchDockerfileWithCancellation(parseResult []instructions.BuildableStage, metaArgs []instructions.ArgCommand, escapeToken rune, source builder.Source) (*dispatchState, error) {

	totalCommands := len(metaArgs)
	currentCommandIndex := 1
	for _, stage := range parseResult {
		totalCommands += len(stage.Commands)
	}
	shlex := NewShellLex(escapeToken)
	for _, meta := range metaArgs {

		fmt.Fprintf(b.Stdout, stepFormat, currentCommandIndex, totalCommands, &meta)
		currentCommandIndex++
		fmt.Fprintln(b.Stdout)

		err := b.processMetaArg(meta, shlex)
		if err != nil {
			return nil, err
		}
	}

	prepedStages := []*stageBuilderAndCommands{}
	for _, stage := range parseResult {
		buildStage, err := b.buildStages.add(stage.Name)
		if err != nil {
			return nil, err
		}
		prepedStages = append(prepedStages, &stageBuilderAndCommands{builder: newStageBuilder(b, escapeToken, source, buildStage), commands: stage.Commands})
	}
	wg := &sync.WaitGroup{}
	for _, s := range prepedStages {
		stage := s
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, cmd := range stage.commands {
				select {
				case <-b.clientCtx.Done():
					logrus.Debug("Builder: build cancelled!")
					fmt.Fprint(b.Stdout, "Build cancelled\n")
					buildsFailed.WithValues(metricsBuildCanceled).Inc()
					stage.err = errors.New("Build cancelled")
					stage.builder.state.buildStage.fail()
					return
				default:
					// Not cancelled yet, keep going...
				}

				fmt.Fprintf(b.Stdout, stepFormat, currentCommandIndex, totalCommands, cmd)
				currentCommandIndex++
				fmt.Fprintln(b.Stdout)

				if err := stage.builder.dispatch(cmd); err != nil {
					stage.err = err
					stage.builder.state.buildStage.fail()
					return
				}

				stage.builder.updateRunConfig()
				fmt.Fprintf(b.Stdout, " ---> %s\n", stringid.TruncateID(stage.builder.state.imageID))

			}
			if err := emitImageID(b.Aux, stage.builder.state); err != nil {
				stage.err = err
				stage.builder.state.buildStage.fail()
				return
			}
			stage.builder.state.buildStage.complete()
		}()
	}
	wg.Wait()
	if b.options.Remove {
		b.containerManager.RemoveAll(b.Stdout)
	}
	return prepedStages[len(prepedStages)-1].builder.state, prepedStages[len(prepedStages)-1].err
}

func addNodesForLabelOption(dockerfile *parser.Node, labels map[string]string) {
	if len(labels) == 0 {
		return
	}

	node := parser.NodeFromLabels(labels)
	dockerfile.Children = append(dockerfile.Children, node)
}

// BuildFromConfig builds directly from `changes`, treating it as if it were the contents of a Dockerfile
// It will:
// - Call parse.Parse() to get an AST root for the concatenated Dockerfile entries.
// - Do build by calling builder.dispatch() to call all entries' handling routines
//
// BuildFromConfig is used by the /commit endpoint, with the changes
// coming from the query parameter of the same name.
//
// TODO: Remove?
func BuildFromConfig(config *container.Config, changes []string) (*container.Config, error) {
	if len(changes) == 0 {
		return config, nil
	}

	b := newBuilder(context.Background(), builderOptions{
		Options: &types.ImageBuildOptions{NoCache: true},
	})

	dockerfile, err := parser.Parse(bytes.NewBufferString(strings.Join(changes, "\n")))
	if err != nil {
		return nil, err
	}

	// ensure that the commands are valid
	for _, n := range dockerfile.AST.Children {
		if !validCommitCommands[n.Value] {
			return nil, fmt.Errorf("%s is not a valid change command", n.Value)
		}
	}

	b.Stdout = ioutil.Discard
	b.Stderr = ioutil.Discard
	b.disableCommit = true

	// dispatchState := newDispatchState()
	// dispatchState.runConfig = config
	// return dispatchFromDockerfile(b, dockerfile, dispatchState)

	stage := instructions.BuildableStage{
		Name: "0",
		Commands: []interface{}{&instructions.ResumeBuildCommand{
			BaseConfig: config,
		}},
	}

	for _, n := range dockerfile.AST.Children {
		cmd, err := instructions.ParseCommand(n)
		if err != nil {
			if instructions.IsUnknownInstruction(err) {
				buildsFailed.WithValues(metricsUnknownInstructionError).Inc()
			}
			return nil, err
		}
		stage.AddCommand(cmd)
	}

	parseState := []instructions.BuildableStage{
		stage,
	}
	b.buildArgs.ResetAllowed()
	res, err := b.dispatchDockerfileWithCancellation(parseState, nil, dockerfile.EscapeToken, nil)

	if err != nil {
		return nil, err
	}
	return res.runConfig, nil
}
