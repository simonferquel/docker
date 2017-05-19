package buildtree

import (
	"bytes"
	"context"
	"io"
	"sort"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/pkg/errors"

	"strconv"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/builder"
	"github.com/docker/docker/pkg/signal"
)

type BuildOptions struct {
	Options       *types.ImageBuildOptions
	Ctx           context.Context
	DisableCommit bool
	NoCache       bool

	ImageCache   builder.ImageCache
	ImageSources builder.ImageMounts

	Docker builder.Backend
	Output io.Writer
	Stdout io.Writer
	Stderr io.Writer

	BuildArgs *builder.BuildArgs

	Source builder.Source
}

// BuildTree represent a complete build operation
type BuildTree interface {
	// SetTargetStage specifies which stage is the target of the build operation
	// It removes unnecessary stages and validate the target is there
	// If not called prior to build, the last stage is used
	SetTargetStage(name string) error

	// Build actually builds the tree
	Run(options *BuildOptions) (*builder.Result, error)

	AddStage(stage BuildStage) error
	LastStage() BuildStage
	GetStage(nameOrIndex string) (stage BuildStage, ok bool)
	Empty() bool
}

type BuildStage interface {
	Name() string
	addStep(step BuildStep)
	run(ctx *stageContext)
	base() StageOrImage
}

type BuildStep interface {
	run(ctx *stageContext) error
}

type buildTree struct {
	stages   []BuildStage
	stageMap map[string]BuildStage
}

// NewBuildTree initialize an empty build tree
func NewBuildTree() BuildTree {
	return &buildTree{stageMap: make(map[string]BuildStage)}
}

func (t *buildTree) SetTargetStage(name string) error {
	if _, ok := t.stageMap[name]; !ok {
		return errors.Errorf("Stage %s does not exist", name)
	}

	for i, stage := range t.stages {
		if stage.Name() == name {
			toKeep := t.stages[:i+1]
			toRemove := t.stages[i+1:]
			for _, r := range toRemove {
				delete(t.stageMap, r.Name())
			}
			t.stages = toKeep
			break
		}
	}
	return nil
}

type stageAndContext struct {
	context *stageContext
	stage   BuildStage
}

func (t *buildTree) Run(options *BuildOptions) (*builder.Result, error) {

	buildCtx := &buildContext{
		options:        options.Options,
		ctx:            options.Ctx,
		disableCommit:  options.DisableCommit,
		noCache:        options.NoCache,
		cacheBusted:    false,
		imageCache:     options.ImageCache,
		imageSources:   options.ImageSources,
		pathCache:      nil, // TODO: reimplement path cache
		docker:         options.Docker,
		output:         options.Output,
		stdout:         options.Stdout,
		stderr:         options.Stderr,
		stagesContexts: make(map[string]*stageContext),
		buildArgs:      options.BuildArgs,
		source:         options.Source,
	}
	contexts := []stageAndContext{}
	for _, stage := range t.stages {
		stageCtx := &stageContext{
			done:          make(chan interface{}),
			tmpContainers: make(map[string]struct{}),
			commonContext: buildCtx,
		}
		contexts = append(contexts, stageAndContext{stage: stage, context: stageCtx})
		buildCtx.stagesContexts[stage.Name()] = stageCtx
	}

	for _, stage := range contexts {
		s := stage
		// go func() {
		// 	s.stage.run(stage.context)
		// }()
		s.stage.run(stage.context)
	}

	for _, stage := range contexts {
		<-stage.context.done
	}

	lastStage := contexts[len(contexts)-1]
	return lastStage.context.result, lastStage.context.err

}

func (t *buildTree) AddStage(stage BuildStage) error {
	name := stage.Name()
	if name == "" {
		name = strconv.Itoa(len(t.stages))
	}
	if _, ok := t.stageMap[name]; ok {
		return errors.Errorf("Build stage with name %s already exists", name)
	}
	t.stages = append(t.stages, stage)
	t.stageMap[name] = stage
	return nil
}

func (t *buildTree) LastStage() BuildStage {
	if len(t.stages) == 0 {
		return nil
	}
	return t.stages[len(t.stages)-1]
}

func (t *buildTree) GetStage(nameOrIndex string) (stage BuildStage, ok bool) {
	stage, ok = t.stageMap[nameOrIndex]
	return
}

func (t *buildTree) Empty() bool {
	return len(t.stages) == 0
}

type StageOrImage struct {
	Image builder.Image
	Stage BuildStage
}

func (si StageOrImage) RootImage() builder.Image {
	if si.Image != nil {
		return si.Image
	}
	return si.Stage.base().RootImage()
}

type buildStage struct {
	name      string
	baseImage StageOrImage
	steps     []BuildStep
}

func NewBuildStage(name string, baseImage StageOrImage) BuildStage {
	return &buildStage{name: name, baseImage: baseImage}
}

func (s *buildStage) Name() string {
	return s.name
}
func (s *buildStage) addStep(step BuildStep) {
	s.steps = append(s.steps, step)
}

func (s *buildStage) base() StageOrImage {
	return s.baseImage
}

func (s *buildStage) run(ctx *stageContext) {
	for _, step := range s.steps {
		select {
		case <-ctx.commonContext.ctx.Done():
			// cancel
			ctx.err = errors.New("Build canceled")
			close(ctx.done)
			return
		default:
			// continue
		}

		if err := step.run(ctx); err != nil {
			ctx.err = err
			close(ctx.done)
			return
		}
	}
	ctx.result = &builder.Result{FromImage: s.baseImage.RootImage(), ImageID: ctx.imageID}
	close(ctx.done)
}

type envBuildStep struct {
	envAfter      []string
	commitMessage string
}

func NewEnvBuildStep(stage BuildStage, envValues map[string]string, envBefore []string) (envAfter []string, err error) {

	// this one is special: as it has side effect on the parser itself, it must be executed early. Result of the execution is kept for committing,
	// and for maintaining evolution of the env variables at build time
	if stage == nil {
		return nil, errors.New("ENV command must be run after FROM")
	}
	commitMessage := bytes.NewBufferString("ENV")
	envAfter = append(envAfter, envBefore...)
	for name, value := range envValues {
		if len(name) == 0 {
			return nil, errors.New("ENV names can not be blank")
		}

		newVar := name + "=" + value
		commitMessage.WriteString(" " + newVar)

		gotOne := false
		for i, envVar := range envAfter {
			envParts := strings.SplitN(envVar, "=", 2)
			compareFrom := envParts[0]
			if equalEnvKeys(compareFrom, name) {
				envAfter[i] = newVar
				gotOne = true
				break
			}
		}
		if !gotOne {
			envAfter = append(envAfter, newVar)
		}
	}
	stage.addStep(&envBuildStep{commitMessage: commitMessage.String(), envAfter: envAfter})
	return envAfter, nil
}

type maintainerBuildStep struct {
	name string
}

func NewMaintainerBuildStep(stage BuildStage, name string) error {
	if stage == nil {
		return errors.New("MAINTAINER command must be run after FROM")
	}
	stage.addStep(&maintainerBuildStep{name: name})
	return nil
}

type labelsBuildStep struct {
	labels map[string]string
}

func NewLabelsBuildStep(stage BuildStage, labels map[string]string) error {
	if stage == nil {
		return errors.New("LABEL command must be run after FROM")
	}
	stage.addStep(&labelsBuildStep{labels: labels})
	return nil
}

type addBuildStep struct {
	sources []string
	dest    string
}

func NewAddBuildStep(stage BuildStage, sources []string, dest string) error {
	if stage == nil {
		return errors.New("ADD command must be run after FROM")
	}
	stage.addStep(&addBuildStep{sources: sources, dest: dest})
	return nil
}

type copyBuildStep struct {
	addBuildStep
	sourceStageOrImageRef string
}

func NewCopyBuildStep(stage BuildStage, sources []string, dest, stageOrImage string) error {
	if stage == nil {
		return errors.New("COPY command must be run after FROM")
	}
	stage.addStep(&copyBuildStep{addBuildStep: addBuildStep{sources: sources, dest: dest}, sourceStageOrImageRef: stageOrImage})
	return nil
}

type onbuildBuildStep struct {
	expression string
}

func NewOnBuildBuildStep(stage BuildStage, expression string) error {
	if stage == nil {
		return errors.New("ONBUILD command must be run after FROM")
	}
	stage.addStep(&onbuildBuildStep{expression: expression})
	return nil
}

type workdirBuildStep struct {
	wd string
}

func NewWorkdirBuildStep(stage BuildStage, workdir string) error {
	if stage == nil {
		return errors.New("WORKDIR command must be run after FROM")
	}
	stage.addStep(&workdirBuildStep{wd: workdir})
	return nil
}

type runBuildStep struct {
	commandLine  []string
	prependShell bool
}

func NewRunBuildStep(stage BuildStage, cmdLine []string, prependShell bool) error {
	if stage == nil {
		return errors.New("RUN command must be run after FROM")
	}
	stage.addStep(&runBuildStep{commandLine: cmdLine, prependShell: prependShell})
	return nil
}

type cmdBuildStep struct {
	commandLine  []string
	prependShell bool
}

func NewCmdBuildStep(stage BuildStage, cmdLine []string, prependShell bool) error {
	if stage == nil {
		return errors.New("CMD command must be run after FROM")
	}
	stage.addStep(&cmdBuildStep{commandLine: cmdLine, prependShell: prependShell})
	return nil
}

type healthCheckBuildStep struct {
	healthCheck *container.HealthConfig
}

func NewHealthCheckBuildStep(stage BuildStage, healthCheck *container.HealthConfig) error {
	if stage == nil {
		return errors.New("HEALTHCHECK command must be run after FROM")
	}
	stage.addStep(&healthCheckBuildStep{healthCheck: healthCheck})
	return nil
}

type entryPointBuildStep struct {
	disabled     bool
	commandLine  []string
	prependShell bool
}

func NewEntrypointBuildStep(stage BuildStage, disabled bool, cmdLine []string, prependShell bool) error {
	if stage == nil {
		return errors.New("ENTRYPOINT command must be run after FROM")
	}
	stage.addStep(&entryPointBuildStep{disabled: disabled, commandLine: cmdLine, prependShell: prependShell})
	return nil
}

type exposeBuildStep struct {
	exposedPorts []string
}

func NewExposeBuildStep(stage BuildStage, exposedPorts []string) error {
	if stage == nil {
		return errors.New("EXPOSE command must be run after FROM")
	}
	sort.Strings(exposedPorts)
	stage.addStep(&exposeBuildStep{exposedPorts: exposedPorts})
	return nil
}

type userBuildStep struct {
	user string
}

func NewUserBuildStep(stage BuildStage, user string) error {
	if stage == nil {
		return errors.New("USER command must be run after FROM")
	}
	stage.addStep(&userBuildStep{user: user})
	return nil
}

type volummesBuildStep struct {
	volumes []string
}

func NewVolumesBuildStep(stage BuildStage, volumes []string) error {
	if stage == nil {
		return errors.New("VOLUME command must be run after FROM")
	}
	stage.addStep(&volummesBuildStep{volumes: volumes})
	return nil
}

type stopsignalBuildStep struct {
	signal string
}

func NewStopSignalBuildStep(stage BuildStage, sig string) error {

	if stage == nil {
		return errors.New("STOPSIGNAL command must be run after FROM")
	}

	_, err := signal.ParseSignal(sig)
	if err != nil {
		return err
	}
	stage.addStep(&stopsignalBuildStep{signal: sig})
	return nil
}

type argsBuildStep struct {
	name  string
	value *string
}

func NewArgBuildStep(stage BuildStage, name string, value *string) error {
	if stage == nil {
		return errors.New("ARG command must be run after FROM")
	}
	stage.addStep(&argsBuildStep{name: name, value: value})
	return nil
}

type shellBuildStep struct {
	shell []string
}

func NewShellBuildStep(stage BuildStage, shell []string) error {
	if stage == nil {
		return errors.New("SHELL command must be run after FROM")
	}
	stage.addStep(&shellBuildStep{shell: shell})
	return nil
}
