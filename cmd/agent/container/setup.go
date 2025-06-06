//go:build !windows

package container

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/loft-sh/devpod/cmd/flags"

	"github.com/loft-sh/devpod/pkg/agent"
	"github.com/loft-sh/devpod/pkg/agent/tunnel"
	"github.com/loft-sh/devpod/pkg/agent/tunnelserver"
	"github.com/loft-sh/devpod/pkg/command"
	"github.com/loft-sh/devpod/pkg/compress"
	config2 "github.com/loft-sh/devpod/pkg/config"
	"github.com/loft-sh/devpod/pkg/copy"
	"github.com/loft-sh/devpod/pkg/credentials"
	"github.com/loft-sh/devpod/pkg/devcontainer/config"
	"github.com/loft-sh/devpod/pkg/devcontainer/setup"
	"github.com/loft-sh/devpod/pkg/dockercredentials"
	"github.com/loft-sh/devpod/pkg/envfile"
	"github.com/loft-sh/devpod/pkg/extract"
	"github.com/loft-sh/devpod/pkg/git"
	"github.com/loft-sh/devpod/pkg/ide/fleet"
	"github.com/loft-sh/devpod/pkg/ide/jetbrains"
	"github.com/loft-sh/devpod/pkg/ide/jupyter"
	"github.com/loft-sh/devpod/pkg/ide/openvscode"
	"github.com/loft-sh/devpod/pkg/ide/rstudio"
	"github.com/loft-sh/devpod/pkg/ide/vscode"
	provider2 "github.com/loft-sh/devpod/pkg/provider"
	"github.com/loft-sh/devpod/pkg/single"
	"github.com/loft-sh/devpod/pkg/ts"
	"github.com/loft-sh/log"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

var DockerlessImageConfigOutput = "/.dockerless/image.json"

// SetupContainerCmd holds the cmd flags
type SetupContainerCmd struct {
	*flags.GlobalFlags

	ChownWorkspace         bool
	StreamMounts           bool
	InjectGitCredentials   bool
	ContainerWorkspaceInfo string
	SetupInfo              string
	AccessKey              string
	PlatformHost           string
	WorkspaceHost          string
}

// NewSetupContainerCmd creates a new command
func NewSetupContainerCmd(flags *flags.GlobalFlags) *cobra.Command {
	cmd := &SetupContainerCmd{
		GlobalFlags: flags,
	}
	setupContainerCmd := &cobra.Command{
		Use:   "setup",
		Short: "Sets up a container",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			return cmd.Run(context.Background())
		},
	}
	setupContainerCmd.Flags().BoolVar(&cmd.StreamMounts, "stream-mounts", false, "If true, will try to stream the bind mounts from the host")
	setupContainerCmd.Flags().BoolVar(&cmd.ChownWorkspace, "chown-workspace", false, "If DevPod should chown the workspace to the remote user")
	setupContainerCmd.Flags().BoolVar(&cmd.InjectGitCredentials, "inject-git-credentials", false, "If DevPod should inject git credentials during setup")
	setupContainerCmd.Flags().StringVar(&cmd.ContainerWorkspaceInfo, "container-workspace-info", "", "The container workspace info")
	setupContainerCmd.Flags().StringVar(&cmd.SetupInfo, "setup-info", "", "The container setup info")
	setupContainerCmd.Flags().StringVar(&cmd.AccessKey, "access-key", "", "Access Key to use")
	setupContainerCmd.Flags().StringVar(&cmd.WorkspaceHost, "workspace-host", "", "Workspace hostname to use")
	setupContainerCmd.Flags().StringVar(&cmd.PlatformHost, "platform-host", "", "Platform host")
	_ = setupContainerCmd.MarkFlagRequired("setup-info")
	return setupContainerCmd
}

// Run runs the command logic
func (cmd *SetupContainerCmd) Run(ctx context.Context) error {
	// create a grpc client
	tunnelClient, err := tunnelserver.NewTunnelClient(os.Stdin, os.Stdout, true, 0)
	if err != nil {
		return fmt.Errorf("error creating tunnel client: %w", err)
	}

	// create debug logger
	logger := tunnelserver.NewTunnelLogger(ctx, tunnelClient, cmd.Debug)
	logger.Debugf("Created logger")

	// this message serves as a ping to the client
	_, err = tunnelClient.Ping(ctx, &tunnel.Empty{})
	if err != nil {
		return errors.Wrap(err, "ping client")
	}

	// start setting up container
	logger.Debugf("Start setting up container...")
	workspaceInfo, _, err := agent.DecodeContainerWorkspaceInfo(cmd.ContainerWorkspaceInfo)
	if err != nil {
		return err
	}

	decompressed, err := compress.Decompress(cmd.SetupInfo)
	if err != nil {
		return err
	}

	setupInfo := &config.Result{}
	err = json.Unmarshal([]byte(decompressed), setupInfo)
	if err != nil {
		return err
	}

	// sync mounts
	if cmd.StreamMounts {
		mounts := config.GetMounts(setupInfo)
		logger.Debug("Syncing mounts... ", mounts)
		for _, m := range mounts {
			// If we are resetting the workspace and it's sources, always re stream the mounts
			if !workspaceInfo.CLIOptions.Reset {
				files, err := os.ReadDir(m.Target)
				if err == nil && len(files) > 0 {
					logger.Debug("Skip stream mount ", m.Target, " because it's not empty")
					continue
				}
			}

			// stream mount
			err = streamMount(ctx, workspaceInfo, m, tunnelClient, logger)
			if err != nil {
				return err
			}
		}
	}

	// do dockerless build
	err = dockerlessBuild(ctx, setupInfo, &workspaceInfo.Dockerless, tunnelClient, cmd.Debug, logger)
	if err != nil {
		return fmt.Errorf("dockerless build: %w", err)
	}

	// fill container env
	err = fillContainerEnv(setupInfo)
	if err != nil {
		return err
	}

	if cmd.InjectGitCredentials {
		// configure git credentials
		cancelCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		cleanupFunc, err := configureSystemGitCredentials(cancelCtx, cancel, tunnelClient, logger)
		if err != nil {
			logger.Errorf("Error configuring git credentials: %v", err)
		} else {
			defer cleanupFunc()
		}
	}

	if b, err := workspaceInfo.PullFromInsideContainer.Bool(); err == nil && b {
		// check if workspace folder exists and is a git repository.
		// skip cloning if it does
		_, err := os.Stat(filepath.Join(setupInfo.SubstitutionContext.ContainerWorkspaceFolder, ".git"))
		if err == nil && !workspaceInfo.CLIOptions.Recreate {
			logger.Debugf("Workspace repository already checked out %s, skipping clone", setupInfo.SubstitutionContext.ContainerWorkspaceFolder)
		} else {
			if err := agent.CloneRepositoryForWorkspace(ctx,
				&workspaceInfo.Source,
				&workspaceInfo.Agent,
				setupInfo.SubstitutionContext.ContainerWorkspaceFolder,
				"",
				workspaceInfo.CLIOptions,
				true,
				logger,
			); err != nil {
				return err
			}
		}
	}

	// setup container
	err = setup.SetupContainer(ctx, setupInfo, workspaceInfo.CLIOptions.WorkspaceEnv, cmd.ChownWorkspace, &workspaceInfo.CLIOptions.Platform, tunnelClient, logger)
	if err != nil {
		return err
	}

	// install IDE
	err = cmd.installIDE(setupInfo, &workspaceInfo.IDE, logger)
	if err != nil {
		return err
	}

	// start container daemon if necessary
	if !workspaceInfo.CLIOptions.Platform.Enabled && !workspaceInfo.CLIOptions.DisableDaemon && workspaceInfo.ContainerTimeout != "" {
		err = single.Single("devpod.daemon.pid", func() (*exec.Cmd, error) {
			logger.Debugf("Start DevPod Container Daemon with Inactivity Timeout %s", workspaceInfo.ContainerTimeout)
			binaryPath, err := os.Executable()
			if err != nil {
				return nil, err
			}

			return exec.Command(binaryPath, "agent", "container", "daemon", "--timeout", workspaceInfo.ContainerTimeout), nil
		})
		if err != nil {
			return err
		}
	}

	out, err := json.Marshal(setupInfo)
	if err != nil {
		return fmt.Errorf("marshal setup info: %w", err)
	}

	_, err = tunnelClient.SendResult(ctx, &tunnel.Message{Message: string(out)})
	if err != nil {
		return fmt.Errorf("send result: %w", err)
	}

	return nil
}

func fillContainerEnv(setupInfo *config.Result) error {
	// set remote-env
	if setupInfo.MergedConfig.RemoteEnv == nil {
		setupInfo.MergedConfig.RemoteEnv = make(map[string]string)
	}

	if _, ok := setupInfo.MergedConfig.RemoteEnv["PATH"]; !ok {
		setupInfo.MergedConfig.RemoteEnv["PATH"] = "${containerEnv:PATH}"
	}

	// merge config
	newMergedConfig := &config.MergedDevContainerConfig{}
	err := config.SubstituteContainerEnv(config.ListToObject(os.Environ()), setupInfo.MergedConfig, newMergedConfig)
	if err != nil {
		return errors.Wrap(err, "substitute container env")
	}
	setupInfo.MergedConfig = newMergedConfig
	return nil
}

func dockerlessBuild(
	ctx context.Context,
	setupInfo *config.Result,
	dockerlessOptions *provider2.ProviderDockerlessOptions,
	client tunnel.TunnelClient,
	debug bool,
	log log.Logger,
) error {
	if os.Getenv("DOCKERLESS") != "true" {
		return nil
	}

	_, err := os.Stat(DockerlessImageConfigOutput)
	if err == nil {
		log.Debugf("Skip dockerless build, because container was built already")
		return nil
	}

	buildContext := os.Getenv("DOCKERLESS_CONTEXT")
	if buildContext == "" {
		log.Debugf("Build context is missing for dockerless build")
		return nil
	}

	// check if build info is there
	fallbackDir := filepath.Join(config.DevPodDockerlessBuildInfoFolder, config.DevPodContextFeatureFolder)
	buildInfoDir := filepath.Join(buildContext, config.DevPodContextFeatureFolder)
	_, err = os.Stat(buildInfoDir)
	if err != nil {
		// try to rename from fallback dir
		err = copy.RenameDirectory(fallbackDir, buildInfoDir)
		if err != nil {
			return fmt.Errorf("rename dir: %w", err)
		}

		_, err = os.Stat(buildInfoDir)
		if err != nil {
			return fmt.Errorf("couldn't find build dir %s: %w", buildInfoDir, err)
		}
	}

	binaryPath, err := os.Executable()
	if err != nil {
		return err
	}

	// configure credentials
	if dockerlessOptions.DisableDockerCredentials != "true" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()

		// configure the docker credentials
		dockerCredentialsDir, err := configureDockerCredentials(ctx, cancel, client, log)
		if err != nil {
			log.Errorf("Error configuring docker credentials: %v", err)
		} else {
			defer func() {
				_ = os.Unsetenv("DOCKER_CONFIG")
				_ = os.RemoveAll(dockerCredentialsDir)
			}()
		}
	}

	// build args
	args := []string{"build", "--ignore-path", binaryPath}
	args = append(args, parseIgnorePaths(dockerlessOptions.IgnorePaths)...)
	args = append(args, "--build-arg", "TARGETOS="+runtime.GOOS)
	args = append(args, "--build-arg", "TARGETARCH="+runtime.GOARCH)
	if dockerlessOptions.RegistryCache != "" {
		log.Debug("Appending registry cache to dockerless build arguments ", dockerlessOptions.RegistryCache)
		args = append(args, "--registry-cache", dockerlessOptions.RegistryCache)
	}

	// ignore mounts
	args = append(args, "--ignore-path", setupInfo.SubstitutionContext.ContainerWorkspaceFolder)
	for _, m := range setupInfo.MergedConfig.Mounts {
		// check if there already, then we don't touch it
		files, err := os.ReadDir(m.Target)
		if err == nil && len(files) > 0 {
			args = append(args, "--ignore-path", m.Target)
		}
	}

	// write output to log
	errWriter := log.Writer(logrus.InfoLevel, false)
	defer errWriter.Close()

	// start building
	log.Infof("Start dockerless building %s %s", "/.dockerless/dockerless", strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, "/.dockerless/dockerless", args...)

	if debug {
		debugWriter := log.Writer(logrus.DebugLevel, false)
		defer debugWriter.Close()
		cmd.Stdout = debugWriter
	}
	cmd.Stderr = errWriter
	cmd.Env = os.Environ()
	err = cmd.Run()
	if err != nil {
		return err
	}

	// add container env to envfile.json
	rawConfig, err := os.ReadFile(DockerlessImageConfigOutput)
	if err != nil {
		return err
	}

	// parse config file
	configFile := &v1.ConfigFile{}
	err = json.Unmarshal(rawConfig, configFile)
	if err != nil {
		return fmt.Errorf("parse container config: %w", err)
	}

	// apply env
	envfile.MergeAndApply(config.ListToObject(configFile.Config.Env), log)

	// rename build path
	_ = os.RemoveAll(fallbackDir)
	err = copy.RenameDirectory(buildInfoDir, fallbackDir)
	if err != nil {
		log.Debugf("Error renaming dir %s: %v", buildInfoDir, err)
		return nil
	}

	return nil
}

func parseIgnorePaths(ignorePaths string) []string {
	if strings.TrimSpace(ignorePaths) == "" {
		return nil
	}

	retPaths := []string{}
	splitted := strings.Split(ignorePaths, ",")
	for _, s := range splitted {
		retPaths = append(retPaths, "--ignore-path", strings.TrimSpace(s))
	}

	return retPaths
}

func configureDockerCredentials(
	ctx context.Context,
	cancel context.CancelFunc,
	client tunnel.TunnelClient,
	log log.Logger,
) (string, error) {
	serverPort, err := credentials.StartCredentialsServer(ctx, cancel, client, log)
	if err != nil {
		return "", err
	}

	dockerCredentials, err := dockercredentials.ConfigureCredentialsDockerless("/.dockerless/.docker", serverPort, log)
	if err != nil {
		return "", err
	}

	return dockerCredentials, nil
}

func (cmd *SetupContainerCmd) installIDE(setupInfo *config.Result, ide *provider2.WorkspaceIDEConfig, log log.Logger) error {
	switch ide.Name {
	case string(config2.IDENone):
		return nil
	case string(config2.IDEVSCode):
		return cmd.setupVSCode(setupInfo, ide.Options, vscode.FlavorStable, log)
	case string(config2.IDEVSCodeInsiders):
		return cmd.setupVSCode(setupInfo, ide.Options, vscode.FlavorInsiders, log)
	case string(config2.IDECursor):
		return cmd.setupVSCode(setupInfo, ide.Options, vscode.FlavorCursor, log)
	case string(config2.IDEPositron):
		return cmd.setupVSCode(setupInfo, ide.Options, vscode.FlavorPositron, log)
	case string(config2.IDECodium):
		return cmd.setupVSCode(setupInfo, ide.Options, vscode.FlavorCodium, log)
	case string(config2.IDEWindsurf):
		return cmd.setupVSCode(setupInfo, ide.Options, vscode.FlavorWindsurf, log)
	case string(config2.IDEOpenVSCode):
		return cmd.setupOpenVSCode(setupInfo, ide.Options, log)
	case string(config2.IDEGoland):
		return jetbrains.NewGolandServer(config.GetRemoteUser(setupInfo), ide.Options, log).Install()
	case string(config2.IDERustRover):
		return jetbrains.NewRustRoverServer(config.GetRemoteUser(setupInfo), ide.Options, log).Install()
	case string(config2.IDEPyCharm):
		return jetbrains.NewPyCharmServer(config.GetRemoteUser(setupInfo), ide.Options, log).Install()
	case string(config2.IDEPhpStorm):
		return jetbrains.NewPhpStorm(config.GetRemoteUser(setupInfo), ide.Options, log).Install()
	case string(config2.IDEIntellij):
		return jetbrains.NewIntellij(config.GetRemoteUser(setupInfo), ide.Options, log).Install()
	case string(config2.IDECLion):
		return jetbrains.NewCLionServer(config.GetRemoteUser(setupInfo), ide.Options, log).Install()
	case string(config2.IDERider):
		return jetbrains.NewRiderServer(config.GetRemoteUser(setupInfo), ide.Options, log).Install()
	case string(config2.IDERubyMine):
		return jetbrains.NewRubyMineServer(config.GetRemoteUser(setupInfo), ide.Options, log).Install()
	case string(config2.IDEWebStorm):
		return jetbrains.NewWebStormServer(config.GetRemoteUser(setupInfo), ide.Options, log).Install()
	case string(config2.IDEDataSpell):
		return jetbrains.NewDataSpellServer(config.GetRemoteUser(setupInfo), ide.Options, log).Install()
	case string(config2.IDEFleet):
		return fleet.NewFleetServer(config.GetRemoteUser(setupInfo), ide.Options, log).Install(setupInfo.SubstitutionContext.ContainerWorkspaceFolder)
	case string(config2.IDEJupyterNotebook):
		return jupyter.NewJupyterNotebookServer(setupInfo.SubstitutionContext.ContainerWorkspaceFolder, config.GetRemoteUser(setupInfo), ide.Options, log).Install()
	case string(config2.IDERStudio):
		err := rstudio.NewRStudioServer(setupInfo.SubstitutionContext.ContainerWorkspaceFolder, config.GetRemoteUser(setupInfo), ide.Options, log).Install()
		if err != nil {
			log.Errorf("could not install rstudio with error: %w", err)
		}
	}

	return nil
}

func (cmd *SetupContainerCmd) setupVSCode(setupInfo *config.Result, ideOptions map[string]config2.OptionValue, flavor vscode.Flavor, log log.Logger) error {
	log.Debugf("Setup %s...", flavor.DisplayName())
	vsCodeConfiguration := config.GetVSCodeConfiguration(setupInfo.MergedConfig)
	settings := ""
	if len(vsCodeConfiguration.Settings) > 0 {
		out, err := json.Marshal(vsCodeConfiguration.Settings)
		if err != nil {
			return err
		}

		settings = string(out)
	}

	user := config.GetRemoteUser(setupInfo)
	err := vscode.NewVSCodeServer(vsCodeConfiguration.Extensions, settings, user, ideOptions, flavor, log).Install()
	if err != nil {
		return err
	}

	// don't install code-server if we don't have settings or extensions
	if len(vsCodeConfiguration.Settings) == 0 && len(vsCodeConfiguration.Extensions) == 0 {
		return nil
	}

	if len(vsCodeConfiguration.Extensions) == 0 {
		return nil
	}

	return single.Single(fmt.Sprintf("%s-async.pid", flavor), func() (*exec.Cmd, error) {
		log.Infof("Install extensions '%s' in the background", strings.Join(vsCodeConfiguration.Extensions, ","))
		binaryPath, err := os.Executable()
		if err != nil {
			return nil, err
		}

		args := []string{
			"agent", "container", "vscode-async",
			"--setup-info", cmd.SetupInfo,
			"--release-channel", string(flavor),
		}

		return exec.Command(binaryPath, args...), nil
	})
}

func (cmd *SetupContainerCmd) setupOpenVSCode(setupInfo *config.Result, ideOptions map[string]config2.OptionValue, log log.Logger) error {
	log.Debugf("Setup openvscode...")
	vsCodeConfiguration := config.GetVSCodeConfiguration(setupInfo.MergedConfig)
	settings := ""
	if len(vsCodeConfiguration.Settings) > 0 {
		out, err := json.Marshal(vsCodeConfiguration.Settings)
		if err != nil {
			return err
		}

		settings = string(out)
	}

	user := config.GetRemoteUser(setupInfo)
	openVSCode := openvscode.NewOpenVSCodeServer(vsCodeConfiguration.Extensions, settings, user, "0.0.0.0", strconv.Itoa(openvscode.DefaultVSCodePort), ideOptions, log)

	// install open vscode
	err := openVSCode.Install()
	if err != nil {
		return err
	}

	// install extensions in background
	if len(vsCodeConfiguration.Extensions) > 0 {
		err = single.Single("openvscode-async.pid", func() (*exec.Cmd, error) {
			log.Infof("Install extensions '%s' in the background", strings.Join(vsCodeConfiguration.Extensions, ","))
			binaryPath, err := os.Executable()
			if err != nil {
				return nil, err
			}

			return exec.Command(binaryPath, "agent", "container", "openvscode-async", "--setup-info", cmd.SetupInfo), nil
		})
		if err != nil {
			return errors.Wrap(err, "install extensions")
		}
	}

	// start the server in the background
	return openVSCode.Start()
}

func configureSystemGitCredentials(ctx context.Context, cancel context.CancelFunc, client tunnel.TunnelClient, log log.Logger) (func(), error) {
	if !command.Exists("git") {
		return nil, errors.New("git not found")
	}

	serverPort, err := credentials.StartCredentialsServer(ctx, cancel, client, log)
	if err != nil {
		return nil, err
	}

	binaryPath, err := os.Executable()
	if err != nil {
		return nil, err
	}

	gitCredentials := fmt.Sprintf("!'%s' agent git-credentials --port %d", binaryPath, serverPort)
	_ = os.Setenv("DEVPOD_GIT_HELPER_PORT", strconv.Itoa(serverPort))

	err = git.CommandContext(ctx, git.GetDefaultExtraEnv(false), "config", "--system", "--add", "credential.helper", gitCredentials).Run()
	if err != nil {
		return nil, fmt.Errorf("add git credential helper: %w", err)
	}

	cleanup := func() {
		log.Debug("Unset setup system credential helper")
		err = git.CommandContext(ctx, git.GetDefaultExtraEnv(false), "config", "--system", "--unset", "credential.helper").Run()
		if err != nil {
			log.Errorf("unset system credential helper %v", err)
		}
	}

	return cleanup, nil
}

func streamMount(ctx context.Context, workspaceInfo *provider2.ContainerWorkspaceInfo, m *config.Mount, tunnelClient tunnel.TunnelClient, logger log.Logger) error {
	// if we have a platform workspace socket we connect directly to it
	if workspaceInfo.CLIOptions.Platform.Enabled {
		// check if the runner proxy socket exists
		httpClient := &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true,
				},
			},
		}

		// build the url
		logger.Infof("Download %s into DevContainer %s", m.Source, m.Target)
		url := fmt.Sprintf(
			"https://%s/kubernetes/management/apis/management.loft.sh/v1/namespaces/%s/devpodworkspaceinstances/%s/download?path=%s",
			ts.RemoveProtocol(workspaceInfo.CLIOptions.Platform.PlatformHost),
			workspaceInfo.CLIOptions.Platform.InstanceNamespace,
			workspaceInfo.CLIOptions.Platform.InstanceName,
			url.QueryEscape(m.Source),
		)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return fmt.Errorf("create request: %w", err)
		}
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", workspaceInfo.CLIOptions.Platform.AccessKey))

		// send the request
		resp, err := httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("download workspace: %w", err)
		}
		defer resp.Body.Close()

		// check if the response is ok
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("download workspace: body = %s, status = %s", string(body), resp.Status)
		}

		// create progress reader
		progressReader := &progressReader{
			Reader: resp.Body,
			Log:    logger,
		}

		// target folder
		err = extract.Extract(progressReader, m.Target)
		if err != nil {
			return fmt.Errorf("stream mount %s: %w", m.String(), err)
		}

		return nil
	}

	// stream mount
	logger.Infof("Copy %s into DevContainer %s", m.Source, m.Target)
	stream, err := tunnelClient.StreamMount(ctx, &tunnel.StreamMountRequest{Mount: m.String()})
	if err != nil {
		return fmt.Errorf("init stream mount %s: %w", m.String(), err)
	}

	// target folder
	err = extract.Extract(tunnelserver.NewStreamReader(stream, logger), m.Target)
	if err != nil {
		return fmt.Errorf("stream mount %s: %w", m.String(), err)
	}

	return nil
}

type progressReader struct {
	Reader io.Reader
	Log    log.Logger

	lastMessage time.Time
	bytesRead   int64
}

func (p *progressReader) Read(b []byte) (n int, err error) {
	n, err = p.Reader.Read(b)
	p.bytesRead += int64(n)
	if time.Since(p.lastMessage) > time.Second*4 {
		p.Log.Infof("Downloaded %.2f MB", float64(p.bytesRead)/1024/1024)
		p.lastMessage = time.Now()
	}

	return n, err
}
