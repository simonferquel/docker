package buildtree

func (s *addBuildStep) run(ctx *stageContext) error {
	downloader := newRemoteSourceDownloader(ctx.commonContext.output, ctx.commonContext.stdout)
	copier := copierFromContext(ctx.commonContext, downloader, nil)
	defer copier.Cleanup()
	copyInstruction, err := copier.createCopyInstruction(append(s.sources, s.dest), "ADD")
	if err != nil {
		return err
	}
	copyInstruction.allowLocalDecompression = true

	return ctx.performCopy(copyInstruction)
}
