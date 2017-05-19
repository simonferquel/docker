package buildtree

// internals for handling commands. Covers many areas and a lot of
// non-contiguous functionality. Please read the comments.

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/backend"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/builder"
	"github.com/docker/docker/builder/remotecontext"
	containerpkg "github.com/docker/docker/container"
	"github.com/docker/docker/pkg/httputils"
	"github.com/docker/docker/pkg/ioutils"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/docker/docker/pkg/progress"
	"github.com/docker/docker/pkg/streamformatter"
	"github.com/docker/docker/pkg/stringid"
	"github.com/docker/docker/pkg/system"
	"github.com/pkg/errors"
)

var defaultLogConfig = container.LogConfig{Type: "none"}

func (b *stageContext) commit(comment string) error {
	if b.commonContext.disableCommit {
		return nil
	}

	runConfigWithCommentCmd := copyRunConfig(b.runConfig, withCmdComment(comment))
	hit, err := b.probeCache(runConfigWithCommentCmd)
	if err != nil || hit {
		return err
	}
	id, err := b.create(runConfigWithCommentCmd)
	if err != nil {
		return err
	}

	return b.commitContainer(id, runConfigWithCommentCmd)
}

// TODO: see if any args can be dropped
func (b *stageContext) commitContainer(id string, containerConfig *container.Config) error {
	if b.commonContext.disableCommit {
		return nil
	}

	commitCfg := &backend.ContainerCommitConfig{
		ContainerCommitConfig: types.ContainerCommitConfig{
			Author: b.maintainer,
			Pause:  true,
			// TODO: this should be done by Commit()
			Config: copyRunConfig(b.runConfig),
		},
		ContainerConfig: containerConfig,
	}

	// Commit the container
	imageID, err := b.commonContext.docker.Commit(id, commitCfg)
	if err != nil {
		return err
	}

	b.imageID = imageID
	return nil
}

type runConfigModifier func(*container.Config)

func copyRunConfig(runConfig *container.Config, modifiers ...runConfigModifier) *container.Config {
	copy := *runConfig
	for _, modifier := range modifiers {
		modifier(&copy)
	}
	return &copy
}

func withCmd(cmd []string) runConfigModifier {
	return func(runConfig *container.Config) {
		runConfig.Cmd = cmd
	}
}

// withCmdComment sets Cmd to a nop comment string. See withCmdCommentString for
// why there are two almost identical versions of this.
func withCmdComment(comment string) runConfigModifier {
	return func(runConfig *container.Config) {
		runConfig.Cmd = append(getShell(runConfig), "#(nop) ", comment)
	}
}

// withCmdCommentString exists to maintain compatibility with older versions.
// A few instructions (workdir, copy, add) used a nop comment that is a single arg
// where as all the other instructions used a two arg comment string. This
// function implements the single arg version.
func withCmdCommentString(comment string) runConfigModifier {
	return func(runConfig *container.Config) {
		runConfig.Cmd = append(getShell(runConfig), "#(nop) "+comment)
	}
}

func withEnv(env []string) runConfigModifier {
	return func(runConfig *container.Config) {
		runConfig.Env = env
	}
}

// withEntrypointOverride sets an entrypoint on runConfig if the command is
// not empty. The entrypoint is left unmodified if command is empty.
//
// The dockerfile RUN instruction expect to run without an entrypoint
// so the runConfig entrypoint needs to be modified accordingly. ContainerCreate
// will change a []string{""} entrypoint to nil, so we probe the cache with the
// nil entrypoint.
func withEntrypointOverride(cmd []string, entrypoint []string) runConfigModifier {
	return func(runConfig *container.Config) {
		if len(cmd) > 0 {
			runConfig.Entrypoint = entrypoint
		}
	}
}

// getShell is a helper function which gets the right shell for prefixing the
// shell-form of RUN, ENTRYPOINT and CMD instructions
func getShell(c *container.Config) []string {
	if 0 == len(c.Shell) {
		return append([]string{}, defaultShell[:]...)
	}
	return append([]string{}, c.Shell[:]...)
}

func (b *buildContext) download(srcURL string) (remote builder.Source, p string, err error) {
	// get filename from URL
	u, err := url.Parse(srcURL)
	if err != nil {
		return
	}
	path := filepath.FromSlash(u.Path) // Ensure in platform semantics
	if strings.HasSuffix(path, string(os.PathSeparator)) {
		path = path[:len(path)-1]
	}
	parts := strings.Split(path, string(os.PathSeparator))
	filename := parts[len(parts)-1]
	if filename == "" {
		err = fmt.Errorf("cannot determine filename from url: %s", u)
		return
	}

	// Initiate the download
	resp, err := httputils.Download(srcURL)
	if err != nil {
		return
	}

	// Prepare file in a tmp dir
	tmpDir, err := ioutils.TempDir("", "docker-remote")
	if err != nil {
		return
	}
	defer func() {
		if err != nil {
			os.RemoveAll(tmpDir)
		}
	}()
	tmpFileName := filepath.Join(tmpDir, filename)
	tmpFile, err := os.OpenFile(tmpFileName, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return
	}

	progressOutput := streamformatter.NewJSONProgressOutput(b.output, true)
	progressReader := progress.NewProgressReader(resp.Body, progressOutput, resp.ContentLength, "", "Downloading")
	// Download and dump result to tmp file
	// TODO: add filehash directly
	if _, err = io.Copy(tmpFile, progressReader); err != nil {
		tmpFile.Close()
		return
	}
	fmt.Fprintln(b.stdout)

	// Set the mtime to the Last-Modified header value if present
	// Otherwise just remove atime and mtime
	mTime := time.Time{}

	lastMod := resp.Header.Get("Last-Modified")
	if lastMod != "" {
		// If we can't parse it then just let it default to 'zero'
		// otherwise use the parsed time value
		if parsedMTime, err := http.ParseTime(lastMod); err == nil {
			mTime = parsedMTime
		}
	}

	tmpFile.Close()

	if err = system.Chtimes(tmpFileName, mTime, mTime); err != nil {
		return
	}

	lc, err := remotecontext.NewLazyContext(tmpDir)
	if err != nil {
		return
	}

	return lc, filename, nil
}

var windowsBlacklist = map[string]bool{
	"c:\\":        true,
	"c:\\windows": true,
}

// probeCache checks if cache match can be found for current build instruction.
// If an image is found, probeCache returns `(true, nil)`.
// If no image is found, it returns `(false, nil)`.
// If there is any error, it returns `(false, err)`.
func (b *stageContext) probeCache(runConfig *container.Config) (bool, error) {
	c := b.commonContext.imageCache
	if c == nil || b.commonContext.noCache || b.commonContext.cacheBusted {
		return false, nil
	}
	cache, err := c.GetCache(b.imageID, runConfig)
	if err != nil {
		return false, err
	}
	if len(cache) == 0 {
		logrus.Debugf("[BUILDER] Cache miss: %s", runConfig.Cmd)
		b.commonContext.cacheBusted = true
		return false, nil
	}

	fmt.Fprint(b.commonContext.stdout, " ---> Using cache\n")
	logrus.Debugf("[BUILDER] Use cached version: %s", runConfig.Cmd)
	b.imageID = string(cache)

	return true, nil
}

func (b *stageContext) create(runConfig *container.Config) (string, error) {
	resources := container.Resources{
		CgroupParent: b.commonContext.options.CgroupParent,
		CPUShares:    b.commonContext.options.CPUShares,
		CPUPeriod:    b.commonContext.options.CPUPeriod,
		CPUQuota:     b.commonContext.options.CPUQuota,
		CpusetCpus:   b.commonContext.options.CPUSetCPUs,
		CpusetMems:   b.commonContext.options.CPUSetMems,
		Memory:       b.commonContext.options.Memory,
		MemorySwap:   b.commonContext.options.MemorySwap,
		Ulimits:      b.commonContext.options.Ulimits,
	}

	// TODO: why not embed a hostconfig in builder?
	hostConfig := &container.HostConfig{
		SecurityOpt: b.commonContext.options.SecurityOpt,
		Isolation:   b.commonContext.options.Isolation,
		ShmSize:     b.commonContext.options.ShmSize,
		Resources:   resources,
		NetworkMode: container.NetworkMode(b.commonContext.options.NetworkMode),
		// Set a log config to override any default value set on the daemon
		LogConfig:  defaultLogConfig,
		ExtraHosts: b.commonContext.options.ExtraHosts,
	}

	// Create the container
	c, err := b.commonContext.docker.ContainerCreate(types.ContainerCreateConfig{
		Config:     runConfig,
		HostConfig: hostConfig,
	})
	if err != nil {
		return "", err
	}
	for _, warning := range c.Warnings {
		fmt.Fprintf(b.commonContext.stdout, " ---> [Warning] %s\n", warning)
	}

	b.tmpContainers[c.ID] = struct{}{}
	fmt.Fprintf(b.commonContext.stdout, " ---> Running in %s\n", stringid.TruncateID(c.ID))
	return c.ID, nil
}

var errCancelled = errors.New("build cancelled")

func (b *stageContext) run(cID string, cmd []string) (err error) {
	attached := make(chan struct{})
	errCh := make(chan error)
	go func() {
		errCh <- b.commonContext.docker.ContainerAttachRaw(cID, nil, b.commonContext.stdout, b.commonContext.stderr, true, attached)
	}()

	select {
	case err := <-errCh:
		return err
	case <-attached:
	}

	finished := make(chan struct{})
	cancelErrCh := make(chan error, 1)
	go func() {
		select {
		case <-b.commonContext.ctx.Done():
			logrus.Debugln("Build cancelled, killing and removing container:", cID)
			b.commonContext.docker.ContainerKill(cID, 0)
			b.removeContainer(cID)
			cancelErrCh <- errCancelled
		case <-finished:
			cancelErrCh <- nil
		}
	}()

	if err := b.commonContext.docker.ContainerStart(cID, nil, "", ""); err != nil {
		close(finished)
		if cancelErr := <-cancelErrCh; cancelErr != nil {
			logrus.Debugf("Build cancelled (%v) and got an error from ContainerStart: %v",
				cancelErr, err)
		}
		return err
	}

	// Block on reading output from container, stop on err or chan closed
	if err := <-errCh; err != nil {
		close(finished)
		if cancelErr := <-cancelErrCh; cancelErr != nil {
			logrus.Debugf("Build cancelled (%v) and got an error from errCh: %v",
				cancelErr, err)
		}
		return err
	}

	waitC, err := b.commonContext.docker.ContainerWait(b.commonContext.ctx, cID, containerpkg.WaitConditionNotRunning)
	if err != nil {
		// Unable to begin waiting for container.
		close(finished)
		if cancelErr := <-cancelErrCh; cancelErr != nil {
			logrus.Debugf("Build cancelled (%v) and unable to begin ContainerWait: %d", cancelErr, err)
		}
		return err
	}

	if status := <-waitC; status.ExitCode() != 0 {
		close(finished)
		if cancelErr := <-cancelErrCh; cancelErr != nil {
			logrus.Debugf("Build cancelled (%v) and got a non-zero code from ContainerWait: %d", cancelErr, status.ExitCode())
		}
		// TODO: change error type, because jsonmessage.JSONError assumes HTTP
		return &jsonmessage.JSONError{
			Message: fmt.Sprintf("The command '%s' returned a non-zero code: %d", strings.Join(cmd, " "), status.ExitCode()),
			Code:    status.ExitCode(),
		}
	}
	close(finished)
	return <-cancelErrCh
}

func (b *stageContext) removeContainer(c string) error {
	rmConfig := &types.ContainerRmConfig{
		ForceRemove:  true,
		RemoveVolume: true,
	}
	if err := b.commonContext.docker.ContainerRm(c, rmConfig); err != nil {
		fmt.Fprintf(b.commonContext.stdout, "Error removing intermediate container %s: %v\n", stringid.TruncateID(c), err)
		return err
	}
	return nil
}

func (b *stageContext) clearTmp() {
	for c := range b.tmpContainers {
		if err := b.removeContainer(c); err != nil {
			return
		}
		delete(b.tmpContainers, c)
		fmt.Fprintf(b.commonContext.stdout, "Removing intermediate container %s\n", stringid.TruncateID(c))
	}
}

// For backwards compat, if there's just one info then use it as the
// cache look-up string, otherwise hash 'em all into one
func getSourceHashFromInfos(infos []copyInfo) string {
	if len(infos) == 1 {
		return infos[0].hash
	}
	var hashs []string
	for _, info := range infos {
		hashs = append(hashs, info.hash)
	}
	return hashStringSlice("multi", hashs)
}

func (b *stageContext) performCopy(inst copyInstruction) error {
	srcHash := getSourceHashFromInfos(inst.infos)

	// TODO: should this have been using origPaths instead of srcHash in the comment?
	runConfigWithCommentCmd := copyRunConfig(
		b.runConfig,
		withCmdCommentString(fmt.Sprintf("%s %s in %s ", inst.cmdName, srcHash, inst.dest)))
	if hit, err := b.probeCache(runConfigWithCommentCmd); err != nil || hit {
		return err
	}

	container, err := b.commonContext.docker.ContainerCreate(types.ContainerCreateConfig{
		Config: runConfigWithCommentCmd,
		// Set a log config to override any default value set on the daemon
		HostConfig: &container.HostConfig{LogConfig: defaultLogConfig},
	})
	if err != nil {
		return err
	}
	b.tmpContainers[container.ID] = struct{}{}

	// Twiddle the destination when it's a relative path - meaning, make it
	// relative to the WORKINGDIR
	dest, err := normaliseDest(inst.cmdName, b.runConfig.WorkingDir, inst.dest)
	if err != nil {
		return err
	}

	for _, info := range inst.infos {
		if err := b.commonContext.docker.CopyOnBuild(container.ID, dest, info.root, info.path, inst.allowLocalDecompression); err != nil {
			return err
		}
	}
	return b.commitContainer(container.ID, runConfigWithCommentCmd)
}
