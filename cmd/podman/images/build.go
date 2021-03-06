package images

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/containers/buildah"
	"github.com/containers/buildah/imagebuildah"
	buildahCLI "github.com/containers/buildah/pkg/cli"
	"github.com/containers/buildah/pkg/parse"
	"github.com/containers/common/pkg/completion"
	"github.com/containers/common/pkg/config"
	"github.com/containers/podman/v2/cmd/podman/common"
	"github.com/containers/podman/v2/cmd/podman/registry"
	"github.com/containers/podman/v2/cmd/podman/utils"
	"github.com/containers/podman/v2/pkg/domain/entities"
	"github.com/docker/go-units"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

// buildFlagsWrapper are local to cmd/ as the build code is using Buildah-internal
// types.  Hence, after parsing, we are converting buildFlagsWrapper to the entities'
// options which essentially embed the Buildah types.
type buildFlagsWrapper struct {
	// Buildah stuff first
	buildahCLI.BudResults
	buildahCLI.LayerResults
	buildahCLI.FromAndBudResults
	buildahCLI.NameSpaceResults
	buildahCLI.UserNSResults

	// SquashAll squashes all layers into a single layer.
	SquashAll bool
}

var (
	// Command: podman _diff_ Object_ID
	buildDescription = "Builds an OCI or Docker image using instructions from one or more Containerfiles and a specified build context directory."
	buildCmd         = &cobra.Command{
		Use:               "build [options] [CONTEXT]",
		Short:             "Build an image using instructions from Containerfiles",
		Long:              buildDescription,
		Args:              cobra.MaximumNArgs(1),
		RunE:              build,
		ValidArgsFunction: common.AutocompleteDefaultOneArg,
		Example: `podman build .
  podman build --creds=username:password -t imageName -f Containerfile.simple .
  podman build --layers --force-rm --tag imageName .`,
	}

	imageBuildCmd = &cobra.Command{
		Args:              buildCmd.Args,
		Use:               buildCmd.Use,
		Short:             buildCmd.Short,
		Long:              buildCmd.Long,
		RunE:              buildCmd.RunE,
		ValidArgsFunction: buildCmd.ValidArgsFunction,
		Example: `podman image build .
  podman image build --creds=username:password -t imageName -f Containerfile.simple .
  podman image build --layers --force-rm --tag imageName .`,
	}

	buildOpts = buildFlagsWrapper{}
)

// useLayers returns false if BUILDAH_LAYERS is set to "0" or "false"
// otherwise it returns true
func useLayers() string {
	layers := os.Getenv("BUILDAH_LAYERS")
	if strings.ToLower(layers) == "false" || layers == "0" {
		return "false"
	}
	return "true"
}

func init() {
	registry.Commands = append(registry.Commands, registry.CliCommand{
		Mode:    []entities.EngineMode{entities.ABIMode, entities.TunnelMode},
		Command: buildCmd,
	})
	buildFlags(buildCmd)

	registry.Commands = append(registry.Commands, registry.CliCommand{
		Mode:    []entities.EngineMode{entities.ABIMode, entities.TunnelMode},
		Command: imageBuildCmd,
		Parent:  imageCmd,
	})
	buildFlags(imageBuildCmd)
}

func buildFlags(cmd *cobra.Command) {
	flags := cmd.Flags()

	// Podman flags
	flags.BoolVarP(&buildOpts.SquashAll, "squash-all", "", false, "Squash all layers into a single layer")

	// Bud flags
	budFlags := buildahCLI.GetBudFlags(&buildOpts.BudResults)

	// --pull flag
	flag := budFlags.Lookup("pull")
	if err := flag.Value.Set("true"); err != nil {
		logrus.Errorf("unable to set --pull to true: %v", err)
	}
	flag.DefValue = "true"
	flags.AddFlagSet(&budFlags)
	// Add the completion functions
	budCompletions := buildahCLI.GetBudFlagsCompletions()
	completion.CompleteCommandFlags(cmd, budCompletions)

	// Layer flags
	layerFlags := buildahCLI.GetLayerFlags(&buildOpts.LayerResults)
	// --layers flag
	flag = layerFlags.Lookup("layers")
	useLayersVal := useLayers()
	buildOpts.Layers = useLayersVal == "true"
	if err := flag.Value.Set(useLayersVal); err != nil {
		logrus.Errorf("unable to set --layers to %v: %v", useLayersVal, err)
	}
	flag.DefValue = useLayersVal
	// --force-rm flag
	flag = layerFlags.Lookup("force-rm")
	if err := flag.Value.Set("true"); err != nil {
		logrus.Errorf("unable to set --force-rm to true: %v", err)
	}
	flag.DefValue = "true"
	flags.AddFlagSet(&layerFlags)

	// FromAndBud flags
	fromAndBudFlags, err := buildahCLI.GetFromAndBudFlags(&buildOpts.FromAndBudResults, &buildOpts.UserNSResults, &buildOpts.NameSpaceResults)
	if err != nil {
		logrus.Errorf("error setting up build flags: %v", err)
		os.Exit(1)
	}
	// --http-proxy flag
	// containers.conf defaults to true but we want to force false by default for remote, since settings do not apply
	if registry.IsRemote() {
		flag = fromAndBudFlags.Lookup("http-proxy")
		buildOpts.HTTPProxy = false
		if err := flag.Value.Set("false"); err != nil {
			logrus.Errorf("unable to set --https-proxy to %v: %v", false, err)
		}
		flag.DefValue = "false"
	}
	flags.AddFlagSet(&fromAndBudFlags)
	// Add the completion functions
	fromAndBudFlagsCompletions := buildahCLI.GetFromAndBudFlagsCompletions()
	completion.CompleteCommandFlags(cmd, fromAndBudFlagsCompletions)
	_ = flags.MarkHidden("signature-policy")
	flags.SetNormalizeFunc(buildahCLI.AliasFlags)
}

// build executes the build command.
func build(cmd *cobra.Command, args []string) error {
	if (cmd.Flags().Changed("squash") && cmd.Flags().Changed("layers")) ||
		(cmd.Flags().Changed("squash-all") && cmd.Flags().Changed("layers")) ||
		(cmd.Flags().Changed("squash-all") && cmd.Flags().Changed("squash")) {
		return errors.New("cannot specify --squash, --squash-all and --layers options together")
	}

	// Extract container files from the CLI (i.e., --file/-f) first.
	var containerFiles []string
	for _, f := range buildOpts.File {
		if f == "-" {
			containerFiles = append(containerFiles, "/dev/stdin")
		} else {
			containerFiles = append(containerFiles, f)
		}
	}

	// Determine context directory.
	var contextDir string
	if len(args) > 0 {
		// The context directory could be a URL.  Try to handle that.
		tempDir, subDir, err := imagebuildah.TempDirForURL("", "buildah", args[0])
		if err != nil {
			return errors.Wrapf(err, "error prepping temporary context directory")
		}
		if tempDir != "" {
			// We had to download it to a temporary directory.
			// Delete it later.
			defer func() {
				if err = os.RemoveAll(tempDir); err != nil {
					logrus.Errorf("error removing temporary directory %q: %v", contextDir, err)
				}
			}()
			contextDir = filepath.Join(tempDir, subDir)
		} else {
			// Nope, it was local.  Use it as is.
			absDir, err := filepath.Abs(args[0])
			if err != nil {
				return errors.Wrapf(err, "error determining path to directory %q", args[0])
			}
			contextDir = absDir
		}
	} else {
		// No context directory or URL was specified.  Try to use the home of
		// the first locally-available Containerfile.
		for i := range containerFiles {
			if strings.HasPrefix(containerFiles[i], "http://") ||
				strings.HasPrefix(containerFiles[i], "https://") ||
				strings.HasPrefix(containerFiles[i], "git://") ||
				strings.HasPrefix(containerFiles[i], "github.com/") {
				continue
			}
			absFile, err := filepath.Abs(containerFiles[i])
			if err != nil {
				return errors.Wrapf(err, "error determining path to file %q", containerFiles[i])
			}
			contextDir = filepath.Dir(absFile)
			break
		}
	}

	if contextDir == "" {
		return errors.Errorf("no context directory and no Containerfile specified")
	}
	if !utils.IsDir(contextDir) {
		return errors.Errorf("context must be a directory: %q", contextDir)
	}
	if len(containerFiles) == 0 {
		if utils.FileExists(filepath.Join(contextDir, "Containerfile")) {
			containerFiles = append(containerFiles, filepath.Join(contextDir, "Containerfile"))
		} else {
			containerFiles = append(containerFiles, filepath.Join(contextDir, "Dockerfile"))
		}
	}

	var logfile *os.File
	if cmd.Flag("logfile").Changed {
		var err error
		logfile, err = os.OpenFile(buildOpts.Logfile, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer logfile.Close()
	}

	apiBuildOpts, err := buildFlagsWrapperToOptions(cmd, contextDir, &buildOpts, logfile)
	if err != nil {
		return err
	}

	_, err = registry.ImageEngine().Build(registry.GetContext(), containerFiles, *apiBuildOpts)
	return err
}

// buildFlagsWrapperToOptions converts the local build flags to the build options used
// in the API which embed Buildah types used across the build code.  Doing the
// conversion here prevents the API from doing that (redundantly).
//
// TODO: this code should really be in Buildah.
func buildFlagsWrapperToOptions(c *cobra.Command, contextDir string, flags *buildFlagsWrapper, logfile *os.File) (*entities.BuildOptions, error) {
	output := ""
	tags := []string{}
	if c.Flag("tag").Changed {
		tags = flags.Tag
		if len(tags) > 0 {
			output = tags[0]
			tags = tags[1:]
		}
	}

	pullPolicy := imagebuildah.PullIfMissing
	if c.Flags().Changed("pull") && flags.Pull {
		pullPolicy = imagebuildah.PullAlways
	}
	if flags.PullAlways {
		pullPolicy = imagebuildah.PullAlways
	}

	if flags.PullNever {
		pullPolicy = imagebuildah.PullIfMissing
	}

	args := make(map[string]string)
	if c.Flag("build-arg").Changed {
		for _, arg := range flags.BuildArg {
			av := strings.SplitN(arg, "=", 2)
			if len(av) > 1 {
				args[av[0]] = av[1]
			} else {
				delete(args, av[0])
			}
		}
	}
	flags.Layers = buildOpts.Layers

	// `buildah bud --layers=false` acts like `docker build --squash` does.
	// That is all of the new layers created during the build process are
	// condensed into one, any layers present prior to this build are
	// retained without condensing.  `buildah bud --squash` squashes both
	// new and old layers down into one.  Translate Podman commands into
	// Buildah.  Squash invoked, retain old layers, squash new layers into
	// one.
	if c.Flags().Changed("squash") && buildOpts.Squash {
		flags.Squash = false
		flags.Layers = false
	}
	// Squash-all invoked, squash both new and old layers into one.
	if c.Flags().Changed("squash-all") {
		flags.Squash = true
		flags.Layers = false
	}

	var stdout, stderr, reporter *os.File
	stdout = os.Stdout
	stderr = os.Stderr
	reporter = os.Stderr

	if logfile != nil {
		logrus.SetOutput(logfile)
		stdout = logfile
		stderr = logfile
		reporter = logfile
	}

	var memoryLimit, memorySwap int64
	var err error
	if c.Flags().Changed("memory") {
		memoryLimit, err = units.RAMInBytes(flags.Memory)
		if err != nil {
			return nil, err
		}
	}

	if c.Flags().Changed("memory-swap") {
		memorySwap, err = units.RAMInBytes(flags.MemorySwap)
		if err != nil {
			return nil, err
		}
	}

	nsValues, networkPolicy, err := parse.NamespaceOptions(c)
	if err != nil {
		return nil, err
	}

	// `buildah bud --layers=false` acts like `docker build --squash` does.
	// That is all of the new layers created during the build process are
	// condensed into one, any layers present prior to this build are retained
	// without condensing.  `buildah bud --squash` squashes both new and old
	// layers down into one.  Translate Podman commands into Buildah.
	// Squash invoked, retain old layers, squash new layers into one.
	if c.Flags().Changed("squash") && flags.Squash {
		flags.Squash = false
		flags.Layers = false
	}
	// Squash-all invoked, squash both new and old layers into one.
	if c.Flags().Changed("squash-all") {
		flags.Squash = true
		flags.Layers = false
	}

	compression := imagebuildah.Gzip
	if flags.DisableCompression {
		compression = imagebuildah.Uncompressed
	}

	isolation, err := parse.IsolationOption(flags.Isolation)
	if err != nil {
		return nil, err
	}

	usernsOption, idmappingOptions, err := parse.IDMappingOptions(c, isolation)
	if err != nil {
		return nil, err
	}
	nsValues = append(nsValues, usernsOption...)

	systemContext, err := parse.SystemContextFromOptions(c)
	if err != nil {
		return nil, err
	}

	format := ""
	flags.Format = strings.ToLower(flags.Format)
	switch {
	case strings.HasPrefix(flags.Format, buildah.OCI):
		format = buildah.OCIv1ImageManifest
	case strings.HasPrefix(flags.Format, buildah.DOCKER):
		format = buildah.Dockerv2ImageManifest
	default:
		return nil, errors.Errorf("unrecognized image type %q", flags.Format)
	}

	runtimeFlags := []string{}
	for _, arg := range flags.RuntimeFlags {
		runtimeFlags = append(runtimeFlags, "--"+arg)
	}

	containerConfig := registry.PodmanConfig()
	for _, arg := range containerConfig.RuntimeFlags {
		runtimeFlags = append(runtimeFlags, "--"+arg)
	}
	if containerConfig.Engine.CgroupManager == config.SystemdCgroupsManager {
		runtimeFlags = append(runtimeFlags, "--systemd-cgroup")
	}

	opts := imagebuildah.BuildOptions{
		AddCapabilities: flags.CapAdd,
		AdditionalTags:  tags,
		Annotations:     flags.Annotation,
		Architecture:    flags.Arch,
		Args:            args,
		BlobDirectory:   flags.BlobCache,
		CNIConfigDir:    flags.CNIConfigDir,
		CNIPluginPath:   flags.CNIPlugInPath,
		CommonBuildOpts: &buildah.CommonBuildOptions{
			AddHost:      flags.AddHost,
			CPUPeriod:    flags.CPUPeriod,
			CPUQuota:     flags.CPUQuota,
			CPUSetCPUs:   flags.CPUSetCPUs,
			CPUSetMems:   flags.CPUSetMems,
			CPUShares:    flags.CPUShares,
			CgroupParent: flags.CgroupParent,
			HTTPProxy:    flags.HTTPProxy,
			Memory:       memoryLimit,
			MemorySwap:   memorySwap,
			ShmSize:      flags.ShmSize,
			Ulimit:       flags.Ulimit,
			Volumes:      flags.Volumes,
		},
		Compression:      compression,
		ConfigureNetwork: networkPolicy,
		ContextDirectory: contextDir,
		//		DefaultMountsFilePath:   FIXME: this requires global flags to be working!
		Devices:                 flags.Devices,
		DropCapabilities:        flags.CapDrop,
		Err:                     stderr,
		ForceRmIntermediateCtrs: flags.ForceRm,
		IDMappingOptions:        idmappingOptions,
		IIDFile:                 flags.Iidfile,
		Isolation:               isolation,
		Labels:                  flags.Label,
		Layers:                  flags.Layers,
		NamespaceOptions:        nsValues,
		NoCache:                 flags.NoCache,
		OS:                      flags.OS,
		Out:                     stdout,
		Output:                  output,
		OutputFormat:            format,
		PullPolicy:              pullPolicy,
		Quiet:                   flags.Quiet,
		RemoveIntermediateCtrs:  flags.Rm,
		ReportWriter:            reporter,
		Runtime:                 containerConfig.RuntimePath,
		RuntimeArgs:             runtimeFlags,
		SignBy:                  flags.SignBy,
		SignaturePolicyPath:     flags.SignaturePolicy,
		Squash:                  flags.Squash,
		SystemContext:           systemContext,
		Target:                  flags.Target,
		TransientMounts:         flags.Volumes,
	}

	return &entities.BuildOptions{BuildOptions: opts}, nil
}
