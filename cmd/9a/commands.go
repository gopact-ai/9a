package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/gopact-ai/9a/internal/api"
	"github.com/gopact-ai/9a/internal/buildinfo"
	workspacepkg "github.com/gopact-ai/9a/internal/workspace"
	"github.com/spf13/cobra"
)

const (
	commandGroup = "commands"
	utilityGroup = "utilities"
)

type cli struct {
	cwd         string
	call        func(api.Request) (json.RawMessage, error)
	callContext func(context.Context, api.Request) (json.RawMessage, error)
}

func newCLI(cwd string) *cli {
	return &cli{cwd: cwd, callContext: callRPCContext}
}

func (c *cli) invoke(ctx context.Context, request api.Request) (json.RawMessage, error) {
	if c.callContext != nil {
		return c.callContext(ctx, request)
	}
	if c.call == nil {
		return nil, errors.New("local runtime client is unavailable")
	}
	return c.call(request)
}

func newRootCommand(c *cli) *cobra.Command {
	var showVersion bool
	root := &cobra.Command{
		Use:   "9a",
		Short: "Connect integrations and run their capabilities",
		Long: `9a turns an integration manifest into capabilities that humans and agents can
search and run from the current workspace.

Run "9a <command> --help" for command-specific examples and flags.`,
		Example: `  9a connect weather.yaml
  9a search "weather forecast"
  9a run weather/current --input '{"city":"Shanghai"}'`,
		SilenceErrors:              true,
		SilenceUsage:               true,
		DisableAutoGenTag:          true,
		SuggestionsMinimumDistance: 2,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if showVersion {
				return writeVersionOutput(cmd, buildinfo.Version)
			}
			return cmd.Help()
		},
	}
	root.Flags().BoolVar(&showVersion, "version", false, "Print the 9a version")
	root.PersistentFlags().Bool("json", false, "Print machine-readable JSON instead of human-readable output")
	root.AddGroup(
		&cobra.Group{ID: commandGroup, Title: "Commands:"},
		&cobra.Group{ID: utilityGroup, Title: "Utilities:"},
	)
	root.SetHelpCommandGroupID(utilityGroup)

	completion := newCompletionCommand()
	completion.Hidden = true
	version := newVersionCommand()
	version.Hidden = true
	paths, _ := defaultLocalPaths()
	daemon := newDaemonCommand(paths)
	daemon.Hidden = true

	root.AddCommand(
		c.newConnectCommand(),
		c.newSearchCommand(),
		c.newRunCommand(),
		c.newStatusCommand(),
		c.newDisconnectCommand(),
		c.newDoctorCommand(),
		c.newSecretCommand(),
		completion,
		version,
		daemon,
	)
	return root
}

func exactArgs(synopsis string, count int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) == count {
			return nil
		}
		if count == 1 {
			return fmt.Errorf("%s requires exactly one argument: %s", cmd.CommandPath(), synopsis)
		}
		return fmt.Errorf("%s requires exactly %d arguments: %s", cmd.CommandPath(), count, synopsis)
	}
}

func (c *cli) runRequest(cmd *cobra.Command, request api.Request) error {
	data, err := c.invoke(cmd.Context(), request)
	if err != nil {
		var remote *rpcError
		if errors.As(err, &remote) {
			if wantsJSON(cmd) {
				if outputErr := writeMachineError(cmd, remote); outputErr != nil {
					return errors.Join(err, outputErr)
				}
				remote.machineWritten = true
				remote.data = nil
			} else if len(remote.data) > 0 && string(remote.data) != "null" {
				data = remote.data
				if outputErr := writeRemoteErrorData(cmd, request, data); outputErr != nil {
					return errors.Join(err, outputErr)
				}
				remote.data = nil
			}
		}
		return err
	}
	return writeCommandOutput(cmd, request, data)
}

func (c *cli) newConnectCommand() *cobra.Command {
	var guide string
	command := &cobra.Command{
		Use:     "connect [manifest.yaml]",
		Short:   "Connect an integration",
		GroupID: commandGroup,
		Long:    "Validate an integration manifest and connect it to the current workspace. Running this again updates the integration. With no arguments, show the first-connect routes without starting the runtime.",
		Example: `  9a connect weather.yaml
	  9a connect --guide http --json
  9a connect mcp --name local-tools -- /absolute/mcp-server
  9a connect a2a --name research-agent https://agent.example.com`,
		Args: func(cmd *cobra.Command, args []string) error {
			if guide != "" {
				if len(args) != 0 {
					return fmt.Errorf("%s --guide cannot be combined with a manifest", cmd.CommandPath())
				}
				return nil
			}
			if len(args) > 1 {
				return fmt.Errorf("%s accepts at most one manifest: <manifest.yaml>", cmd.CommandPath())
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if guide != "" {
				return writeConnectGuide(cmd, guide)
			}
			if len(args) == 0 {
				return writeConnectRoutes(cmd)
			}
			request, err := connectRequest(args[0], c.cwd)
			if err != nil {
				return err
			}
			return c.runRequest(cmd, request)
		},
	}
	command.Flags().StringVar(&guide, "guide", "", "Print the embedded authoring contract for http, mcp, or a2a")
	command.AddCommand(c.newMCPConnectCommand(), c.newA2AConnectCommand())
	return command
}

func (c *cli) newMCPConnectCommand() *cobra.Command {
	var name string
	command := &cobra.Command{
		Use:     "mcp <absolute-executable>",
		Short:   "Connect a local MCP server",
		Long:    "Connect one local MCP server executable with no arguments.",
		Example: `  9a connect mcp --name <slug> -- /absolute/executable`,
		Args:    exactArgs("<absolute-executable>", 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			request, err := protocolConnectRequest("mcp", name, args[0], c.cwd)
			if err != nil {
				return err
			}
			return c.runRequest(cmd, request)
		},
	}
	command.Flags().StringVar(&name, "name", "", "Canonical integration slug")
	_ = command.MarkFlagRequired("name")
	return command
}

func (c *cli) newA2AConnectCommand() *cobra.Command {
	var name string
	command := &cobra.Command{
		Use:     "a2a <url>",
		Short:   "Connect a remote A2A agent",
		Long:    "Connect an A2A agent over HTTPS. Loopback HTTP is accepted for local development.",
		Example: `  9a connect a2a --name <slug> <url>`,
		Args:    exactArgs("<url>", 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			request, err := protocolConnectRequest("a2a", name, args[0], c.cwd)
			if err != nil {
				return err
			}
			return c.runRequest(cmd, request)
		},
	}
	command.Flags().StringVar(&name, "name", "", "Canonical integration slug")
	_ = command.MarkFlagRequired("name")
	return command
}

func (c *cli) newSearchCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "search <query...>",
		Short:   "Find capabilities",
		GroupID: commandGroup,
		Long:    "Search capabilities available in the current workspace.",
		Example: `  9a search "weather forecast"
  9a search weather temperature --json`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("%s requires at least one search term: <query...>", cmd.CommandPath())
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := workspacepkg.Resolve("", c.cwd)
			if err != nil {
				return err
			}
			request := api.Request{Action: "search", Query: strings.Join(args, " "), Root: root}
			return c.runRequest(cmd, request)
		},
	}
}

func (c *cli) newRunCommand() *cobra.Command {
	var input string
	var approval string
	cmd := &cobra.Command{
		Use:     "run <integration>/<capability>",
		Short:   "Run a capability",
		GroupID: commandGroup,
		Long: `Run a capability with one JSON input source: --input, --input @file, or
stdin. Use --input - to read stdin explicitly. Empty input is treated as {}.`,
		Example: `  9a run weather/current
  9a run weather/current --input '{"city":"Shanghai"}'
  9a run weather/current --input @request.json
  printf '%s\n' '{"city":"Shanghai"}' | 9a run weather/current`,
		Args: exactArgs("<integration>/<capability>", 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			request, err := capabilityRunRequest(args[0], input, cmd.InOrStdin())
			if err != nil {
				return err
			}
			root, err := workspacepkg.Resolve("", c.cwd)
			if err != nil {
				return err
			}
			request.Root = root
			request.Approval = approval
			return c.runRequest(cmd, request)
		},
	}
	cmd.Flags().StringVar(&input, "input", "", "JSON input, @path to a JSON file, or - for stdin")
	cmd.Flags().StringVar(&approval, "approve", "", "Approval token returned by approval_required")
	return cmd
}

func (c *cli) newStatusCommand() *cobra.Command {
	var workspace string
	cmd := &cobra.Command{
		Use:     "status [integration]",
		Short:   "Show readiness",
		GroupID: commandGroup,
		Long:    "Show whether the selected workspace is ready.",
		Example: `  9a status
  9a status weather
  9a status --workspace /work/project --json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			integration := ""
			if len(args) == 1 {
				integration = args[0]
			}
			request, err := statusRequest(workspace, c.cwd, integration)
			if err != nil {
				return err
			}
			return c.runRequest(cmd, request)
		},
	}
	cmd.Flags().StringVar(&workspace, "workspace", "", "Workspace directory (default: discover from current directory)")
	_ = cmd.MarkFlagDirname("workspace")
	return cmd
}

func (c *cli) newDisconnectCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "disconnect <integration>",
		Short:   "Disconnect an integration",
		GroupID: commandGroup,
		Long:    "Disconnect an integration while preserving its manifest in the workspace.",
		Example: "  9a disconnect weather",
		Args:    exactArgs("<integration>", 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			request, err := disconnectRequest(args[0], c.cwd)
			if err != nil {
				return err
			}
			return c.runRequest(cmd, request)
		},
	}
}

func (c *cli) newDoctorCommand() *cobra.Command {
	var workspace string
	var fix bool
	cmd := &cobra.Command{
		Use:     "doctor",
		Short:   "Diagnose local problems",
		GroupID: commandGroup,
		Long:    "Check the current workspace without changing it. --fix repairs the gateway and reconnects stale sources; reconnecting may start a local MCP executable or contact an A2A endpoint.",
		Example: `  9a doctor
  9a doctor --fix`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			request, err := doctorRequest(workspace, c.cwd, fix)
			if err != nil {
				return err
			}
			return c.runRequest(cmd, request)
		},
	}
	cmd.Flags().StringVar(&workspace, "workspace", "", "Workspace directory (default: discover from current directory)")
	cmd.Flags().BoolVar(&fix, "fix", false, "Repair the gateway and reconnect stale integrations")
	_ = cmd.MarkFlagDirname("workspace")
	return cmd
}

func (c *cli) newSecretCommand() *cobra.Command {
	command := &cobra.Command{
		Use:     "secret",
		Short:   "Manage integration secrets",
		GroupID: commandGroup,
		Long:    "Store secret values in the operating system credential store. Values are never accepted as command arguments.",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	set := &cobra.Command{
		Use:     "set <integration>.<key>",
		Short:   "Store a secret from hidden input or stdin",
		Example: "  9a secret set weather.api-token\n  printf '%s' \"$TOKEN\" | 9a secret set weather.api-token",
		Args:    exactArgs("<integration>.<key>", 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			request, err := secretSetRequest(args[0], cmd.InOrStdin(), cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			root, err := workspacepkg.Resolve("", c.cwd)
			if err != nil {
				return err
			}
			request.Root = root
			return c.runRequest(cmd, request)
		},
	}
	list := &cobra.Command{
		Use:     "list [integration]",
		Short:   "List secret names and presence",
		Example: "  9a secret list\n  9a secret list weather",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			integration := ""
			if len(args) == 1 {
				integration = args[0]
			}
			request, err := secretListRequest(integration)
			if err != nil {
				return err
			}
			root, err := workspacepkg.Resolve("", c.cwd)
			if err != nil {
				return err
			}
			request.Root = root
			return c.runRequest(cmd, request)
		},
	}
	unset := &cobra.Command{
		Use:     "unset <integration>.<key>",
		Short:   "Remove a secret",
		Example: "  9a secret unset weather.api-token",
		Args:    exactArgs("<integration>.<key>", 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			request, err := secretUnsetRequest(args[0])
			if err != nil {
				return err
			}
			root, err := workspacepkg.Resolve("", c.cwd)
			if err != nil {
				return err
			}
			request.Root = root
			return c.runRequest(cmd, request)
		},
	}
	command.AddCommand(set, list, unset)
	return command
}

func newCompletionCommand() *cobra.Command {
	return &cobra.Command{
		Use:       "completion <shell>",
		Short:     "Generate shell completion",
		GroupID:   utilityGroup,
		Long:      "Generate a completion script for bash, zsh, fish, or powershell and write it to stdout.",
		Example:   "  source <(9a completion bash)\n  9a completion zsh > \"${fpath[1]}/_9a\"",
		Args:      exactArgs("<shell>", 1),
		ValidArgs: []cobra.Completion{"bash", "zsh", "fish", "powershell"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if wantsJSON(cmd) {
				return fmt.Errorf("--json is not supported for completion scripts")
			}
			switch args[0] {
			case "bash":
				return cmd.Root().GenBashCompletionV2(cmd.OutOrStdout(), true)
			case "zsh":
				return cmd.Root().GenZshCompletion(cmd.OutOrStdout())
			case "fish":
				return cmd.Root().GenFishCompletion(cmd.OutOrStdout(), true)
			case "powershell":
				return cmd.Root().GenPowerShellCompletionWithDesc(cmd.OutOrStdout())
			default:
				return fmt.Errorf("unsupported shell %q: expected bash, zsh, fish, or powershell", args[0])
			}
		},
	}
}

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "version",
		Short:   "Print the 9a version",
		GroupID: utilityGroup,
		Long:    "Print the embedded 9a version. Published binaries receive this value from the release tag.",
		Example: "  9a version",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return writeVersionOutput(cmd, buildinfo.Version)
		},
	}
}
