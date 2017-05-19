package dockerfile

// This file contains the dispatchers for each command. Note that
// `nullDispatch` is not actually a command, but support for commands we parse
// but do nothing with.
//
// See evaluator.go for a higher level discussion of the whole evaluator
// package.

import (
	"fmt"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/builder"
	"github.com/docker/docker/builder/dockerfile/buildtree"
	"github.com/docker/docker/builder/dockerfile/parser"
	"github.com/docker/go-connections/nat"
	"github.com/pkg/errors"
)

// ENV foo bar
//
// Sets the environment variable foo to bar, also makes interpolation
// in the dockerfile available from the next statement on via ${foo}.
//
// Note: env has a side effect on the dispatch state (as env variables can be expanded on following commands)
func env(req dispatchRequest) error {
	if len(req.args) == 0 {
		return errAtLeastOneArgument("ENV")
	}

	if len(req.args)%2 != 0 {
		// should never get here, but just in case
		return errTooManyArguments("ENV")
	}

	if err := req.flags.Parse(); err != nil {
		return err
	}

	kvps := make(map[string]string)

	for j := 0; j < len(req.args); j += 2 {
		kvps[req.args[j]] = req.args[j+1]
	}
	envAfter, err := buildtree.NewEnvBuildStep(req.builder.buildTree.LastStage(), kvps, req.state.env)
	if err != nil {
		return err
	}
	req.state.env = envAfter
	return nil
}

// MAINTAINER some text <maybe@an.email.address>
//
// Sets the maintainer metadata.
func maintainer(req dispatchRequest) error {
	if len(req.args) != 1 {
		return errExactlyOneArgument("MAINTAINER")
	}

	return buildtree.NewMaintainerBuildStep(req.builder.buildTree.LastStage(), req.args[0])
}

// LABEL some json data describing the image
//
// Sets the Label variable foo to bar,
//
func label(req dispatchRequest) error {
	if len(req.args) == 0 {
		return errAtLeastOneArgument("LABEL")
	}
	if len(req.args)%2 != 0 {
		// should never get here, but just in case
		return errTooManyArguments("LABEL")
	}

	if err := req.flags.Parse(); err != nil {
		return err
	}

	labels := make(map[string]string)
	for j := 0; j < len(req.args); j++ {
		name := req.args[j]
		if name == "" {
			return errBlankCommandNames("LABEL")
		}

		value := req.args[j+1]
		labels[name] = value
		j++
	}
	return buildtree.NewLabelsBuildStep(req.builder.buildTree.LastStage(), labels)
}

// ADD foo /path
//
// Add the file 'foo' to '/path'. Tarball and Remote URL (git, http) handling
// exist here. If you do not wish to have this automatic handling, use COPY.
//
func add(req dispatchRequest) error {
	if len(req.args) < 2 {
		return errAtLeastTwoArguments("ADD")
	}

	if err := req.flags.Parse(); err != nil {
		return err
	}

	return buildtree.NewAddBuildStep(req.builder.buildTree.LastStage(), req.args[0:len(req.args)-1], req.args[len(req.args)-1])
}

// COPY foo /path
//
// Same as 'ADD' but without the tar and remote url handling.
//
func dispatchCopy(req dispatchRequest) error {
	if len(req.args) < 2 {
		return errAtLeastTwoArguments("COPY")
	}

	flFrom := req.flags.AddString("from", "")
	if err := req.flags.Parse(); err != nil {
		return err
	}

	return buildtree.NewCopyBuildStep(req.builder.buildTree.LastStage(), req.args[0:len(req.args)-1], req.args[len(req.args)-1], flFrom.Value)
}

// FROM imagename[:tag | @digest] [AS build-stage-name]
//
func from(req dispatchRequest) error {
	stageName, err := parseBuildStageName(req.args)
	if err != nil {
		return err
	}

	if err := req.flags.Parse(); err != nil {
		return err
	}

	req.builder.resetImageCache()
	image, err := req.builder.getFromImage(req.shlex, req.args[0])
	if err != nil {
		return err
	}
	err = req.builder.buildTree.AddStage(buildtree.NewBuildStage(stageName, *image))
	if err != nil {
		return err
	}
	req.builder.buildArgs.ResetAllowed()

	if image.Image != nil {
		baseRunConfig := image.Image.RunConfig()
		if baseRunConfig != nil && len(baseRunConfig.OnBuild) > 0 {
			return processOnBuild(req, baseRunConfig.OnBuild)
		}
	}
	return nil
}

func parseBuildStageName(args []string) (string, error) {
	stageName := ""
	switch {
	case len(args) == 3 && strings.EqualFold(args[1], "as"):
		stageName = strings.ToLower(args[2])
		if ok, _ := regexp.MatchString("^[a-z][a-z0-9-_\\.]*$", stageName); !ok {
			return "", errors.Errorf("invalid name for build stage: %q, name can't start with a number or contain symbols", stageName)
		}
	case len(args) != 1:
		return "", errors.New("FROM requires either one or three arguments")
	}

	return stageName, nil
}

// scratchImage is used as a token for the empty base image. It uses buildStage
// as a convenient implementation of builder.Image, but is not actually a
// buildStage.
var scratchImage builder.Image = &buildStage{}

func (b *Builder) getFromImage(shlex *ShellLex, name string) (*buildtree.StageOrImage, error) {
	substitutionArgs := []string{}
	for key, value := range b.buildArgs.GetAllMeta() {
		substitutionArgs = append(substitutionArgs, key+"="+value)
	}

	name, err := shlex.ProcessWord(name, substitutionArgs)
	if err != nil {
		return nil, err
	}

	if im, ok := b.buildTree.GetStage(name); ok {
		return &buildtree.StageOrImage{Stage: im}, nil
	}

	// Windows cannot support a container with no base image.
	if name == api.NoBaseImageSpecifier {
		if runtime.GOOS == "windows" {
			return nil, errors.New("Windows does not support FROM scratch")
		}
		return &buildtree.StageOrImage{Image: scratchImage}, nil
	}
	imageMount, err := b.imageSources.Get(name)
	if err != nil {
		return nil, err
	}
	return &buildtree.StageOrImage{Image: imageMount.Image()}, nil
}

func processOnBuild(req dispatchRequest, onBuildTriggers []string) error {
	dispatchState := req.state

	// parse the ONBUILD triggers by invoking the parser
	for _, step := range onBuildTriggers {
		dockerfile, err := parser.Parse(strings.NewReader(step))
		if err != nil {
			return err
		}

		for _, n := range dockerfile.AST.Children {
			if err := checkDispatch(n); err != nil {
				return err
			}

			upperCasedCmd := strings.ToUpper(n.Value)
			switch upperCasedCmd {
			case "ONBUILD":
				return errors.New("Chaining ONBUILD via `ONBUILD ONBUILD` isn't allowed")
			case "MAINTAINER", "FROM":
				return errors.Errorf("%s isn't allowed as an ONBUILD trigger", upperCasedCmd)
			}
		}

		if err := dispatchFromDockerfile(req.builder, dockerfile, dispatchState); err != nil {
			return err
		}
	}
	return nil
}

// ONBUILD RUN echo yo
//
// ONBUILD triggers run when the image is used in a FROM statement.
//
// ONBUILD handling has a lot of special-case functionality, the heading in
// evaluator.go and comments around dispatch() in the same file explain the
// special cases. search for 'OnBuild' in internals.go for additional special
// cases.
//
func onbuild(req dispatchRequest) error {
	if len(req.args) == 0 {
		return errAtLeastOneArgument("ONBUILD")
	}

	if err := req.flags.Parse(); err != nil {
		return err
	}

	triggerInstruction := strings.ToUpper(strings.TrimSpace(req.args[0]))
	switch triggerInstruction {
	case "ONBUILD":
		return errors.New("Chaining ONBUILD via `ONBUILD ONBUILD` isn't allowed")
	case "MAINTAINER", "FROM":
		return fmt.Errorf("%s isn't allowed as an ONBUILD trigger", triggerInstruction)
	}

	original := regexp.MustCompile(`(?i)^\s*ONBUILD\s*`).ReplaceAllString(req.original, "")
	return buildtree.NewOnBuildBuildStep(req.builder.buildTree.LastStage(), original)
}

// WORKDIR /tmp
//
// Set the working directory for future RUN/CMD/etc statements.
//
func workdir(req dispatchRequest) error {
	if len(req.args) != 1 {
		return errExactlyOneArgument("WORKDIR")
	}

	err := req.flags.Parse()
	if err != nil {
		return err
	}

	return buildtree.NewWorkdirBuildStep(req.builder.buildTree.LastStage(), req.args[0])
}

// RUN some command yo
//
// run a command and commit the image. Args are automatically prepended with
// the current SHELL which defaults to 'sh -c' under linux or 'cmd /S /C' under
// Windows, in the event there is only one argument The difference in processing:
//
// RUN echo hi          # sh -c echo hi       (Linux)
// RUN echo hi          # cmd /S /C echo hi   (Windows)
// RUN [ "echo", "hi" ] # echo hi
//
func run(req dispatchRequest) error {
	if err := req.flags.Parse(); err != nil {
		return err
	}
	args := handleJSONArgs(req.args, req.attributes)
	return buildtree.NewRunBuildStep(req.builder.buildTree.LastStage(), args, !req.attributes["json"])
}

// CMD foo
//
// Set the default command to run in the container (which may be empty).
// Argument handling is the same as RUN.
//
func cmd(req dispatchRequest) error {
	if err := req.flags.Parse(); err != nil {
		return err
	}

	args := handleJSONArgs(req.args, req.attributes)
	return buildtree.NewCmdBuildStep(req.builder.buildTree.LastStage(), args, !req.attributes["json"])
}

// parseOptInterval(flag) is the duration of flag.Value, or 0 if
// empty. An error is reported if the value is given and less than minimum duration.
func parseOptInterval(f *Flag) (time.Duration, error) {
	s := f.Value
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d < time.Duration(container.MinimumDuration) {
		return 0, fmt.Errorf("Interval %#v cannot be less than %s", f.name, container.MinimumDuration)
	}
	return d, nil
}

// HEALTHCHECK foo
//
// Set the default healthcheck command to run in the container (which may be empty).
// Argument handling is the same as RUN.
//
func healthcheck(req dispatchRequest) error {
	if len(req.args) == 0 {
		return errAtLeastOneArgument("HEALTHCHECK")
	}
	typ := strings.ToUpper(req.args[0])
	args := req.args[1:]
	var healthCheck container.HealthConfig
	if typ == "NONE" {
		if len(args) != 0 {
			return errors.New("HEALTHCHECK NONE takes no arguments")
		}
		test := strslice.StrSlice{typ}
		healthCheck = container.HealthConfig{
			Test: test,
		}
	} else {

		flInterval := req.flags.AddString("interval", "")
		flTimeout := req.flags.AddString("timeout", "")
		flStartPeriod := req.flags.AddString("start-period", "")
		flRetries := req.flags.AddString("retries", "")

		if err := req.flags.Parse(); err != nil {
			return err
		}

		switch typ {
		case "CMD":
			cmdSlice := handleJSONArgs(args, req.attributes)
			if len(cmdSlice) == 0 {
				return errors.New("Missing command after HEALTHCHECK CMD")
			}

			if !req.attributes["json"] {
				typ = "CMD-SHELL"
			}

			healthCheck.Test = strslice.StrSlice(append([]string{typ}, cmdSlice...))
		default:
			return fmt.Errorf("Unknown type %#v in HEALTHCHECK (try CMD)", typ)
		}

		interval, err := parseOptInterval(flInterval)
		if err != nil {
			return err
		}
		healthCheck.Interval = interval

		timeout, err := parseOptInterval(flTimeout)
		if err != nil {
			return err
		}
		healthCheck.Timeout = timeout

		startPeriod, err := parseOptInterval(flStartPeriod)
		if err != nil {
			return err
		}
		healthCheck.StartPeriod = startPeriod

		if flRetries.Value != "" {
			retries, err := strconv.ParseInt(flRetries.Value, 10, 32)
			if err != nil {
				return err
			}
			if retries < 1 {
				return fmt.Errorf("--retries must be at least 1 (not %d)", retries)
			}
			healthCheck.Retries = int(retries)
		} else {
			healthCheck.Retries = 0
		}

	}

	return buildtree.NewHealthCheckBuildStep(req.builder.buildTree.LastStage(), &healthCheck)
}

// ENTRYPOINT /usr/sbin/nginx
//
// Set the entrypoint to /usr/sbin/nginx. Will accept the CMD as the arguments
// to /usr/sbin/nginx. Uses the default shell if not in JSON format.
//
// Handles command processing similar to CMD and RUN, only req.runConfig.Entrypoint
// is initialized at newBuilder time instead of through argument parsing.
//
func entrypoint(req dispatchRequest) error {
	if err := req.flags.Parse(); err != nil {
		return err
	}

	parsed := handleJSONArgs(req.args, req.attributes)

	switch {
	case req.attributes["json"]:
		// ENTRYPOINT ["echo", "hi"]
		return buildtree.NewEntrypointBuildStep(req.builder.buildTree.LastStage(), false, strslice.StrSlice(parsed), false)
	case len(parsed) == 0:
		// ENTRYPOINT []
		return buildtree.NewEntrypointBuildStep(req.builder.buildTree.LastStage(), true, nil, false)
	default:
		// ENTRYPOINT echo hi

		return buildtree.NewEntrypointBuildStep(req.builder.buildTree.LastStage(), false, strslice.StrSlice(parsed), true)
	}

}

// EXPOSE 6667/tcp 7000/tcp
//
// Expose ports for links and port mappings. This all ends up in
// req.runConfig.ExposedPorts for runconfig.
//
func expose(req dispatchRequest) error {
	portsTab := req.args

	if len(req.args) == 0 {
		return errAtLeastOneArgument("EXPOSE")
	}

	if err := req.flags.Parse(); err != nil {
		return err
	}

	ports, _, err := nat.ParsePortSpecs(portsTab)
	if err != nil {
		return err
	}

	portList := make([]string, len(ports))
	var i int
	for port := range ports {
		portList[i] = string(port)
		i++
	}
	return buildtree.NewExposeBuildStep(req.builder.buildTree.LastStage(), portList)
}

// USER foo
//
// Set the user to 'foo' for future commands and when running the
// ENTRYPOINT/CMD at container run time.
//
func user(req dispatchRequest) error {
	if len(req.args) != 1 {
		return errExactlyOneArgument("USER")
	}

	if err := req.flags.Parse(); err != nil {
		return err
	}

	return buildtree.NewUserBuildStep(req.builder.buildTree.LastStage(), req.args[0])
}

// VOLUME /foo
//
// Expose the volume /foo for use. Will also accept the JSON array form.
//
func volume(req dispatchRequest) error {
	if len(req.args) == 0 {
		return errAtLeastOneArgument("VOLUME")
	}

	if err := req.flags.Parse(); err != nil {
		return err
	}

	volumes := []string{}
	for _, v := range req.args {
		v = strings.TrimSpace(v)
		if v == "" {
			return errors.New("VOLUME specified can not be an empty string")
		}
		volumes = append(volumes, v)
	}
	return buildtree.NewVolumesBuildStep(req.builder.buildTree.LastStage(), volumes)
}

// STOPSIGNAL signal
//
// Set the signal that will be used to kill the container.
func stopSignal(req dispatchRequest) error {
	if len(req.args) != 1 {
		return errExactlyOneArgument("STOPSIGNAL")
	}

	return buildtree.NewStopSignalBuildStep(req.builder.buildTree.LastStage(), req.args[0])
}

// ARG name[=value]
//
// Adds the variable foo to the trusted list of variables that can be passed
// to builder using the --build-arg flag for expansion/substitution or passing to 'run'.
// Dockerfile author may optionally set a default value of this variable.
func arg(req dispatchRequest) error {
	if len(req.args) != 1 {
		return errExactlyOneArgument("ARG")
	}

	var (
		name       string
		newValue   string
		hasDefault bool
	)

	arg := req.args[0]
	// 'arg' can just be a name or name-value pair. Note that this is different
	// from 'env' that handles the split of name and value at the parser level.
	// The reason for doing it differently for 'arg' is that we support just
	// defining an arg and not assign it a value (while 'env' always expects a
	// name-value pair). If possible, it will be good to harmonize the two.
	if strings.Contains(arg, "=") {
		parts := strings.SplitN(arg, "=", 2)
		if len(parts[0]) == 0 {
			return errBlankCommandNames("ARG")
		}

		name = parts[0]
		newValue = parts[1]
		hasDefault = true
	} else {
		name = arg
		hasDefault = false
	}

	var value *string
	if hasDefault {
		value = &newValue
	}
	req.builder.buildArgs.AddArg(name, value)

	// Arg before FROM doesn't add a layer (and does not appear in build tree)
	if req.builder.buildTree.Empty() {
		req.builder.buildArgs.AddMetaArg(name, value)
		return nil
	}
	return buildtree.NewArgBuildStep(req.builder.buildTree.LastStage(), name, value)
}

// SHELL powershell -command
//
// Set the non-default shell to use.
func shell(req dispatchRequest) error {
	if err := req.flags.Parse(); err != nil {
		return err
	}
	shellSlice := handleJSONArgs(req.args, req.attributes)
	switch {
	case len(shellSlice) == 0:
		// SHELL []
		return errAtLeastOneArgument("SHELL")
	case req.attributes["json"]:
		// SHELL ["powershell", "-command"]

		return buildtree.NewShellBuildStep(req.builder.buildTree.LastStage(), strslice.StrSlice(shellSlice))
	default:
		// SHELL powershell -command - not JSON
		return errNotJSON("SHELL", req.original)
	}
}

func errAtLeastOneArgument(command string) error {
	return fmt.Errorf("%s requires at least one argument", command)
}

func errExactlyOneArgument(command string) error {
	return fmt.Errorf("%s requires exactly one argument", command)
}

func errAtLeastTwoArguments(command string) error {
	return fmt.Errorf("%s requires at least two arguments", command)
}

func errBlankCommandNames(command string) error {
	return fmt.Errorf("%s names can not be blank", command)
}

func errTooManyArguments(command string) error {
	return fmt.Errorf("Bad input to %s, too many arguments", command)
}
