package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	adapterreg "github.com/gopact-ai/9a/internal/adapter"
	"github.com/gopact-ai/9a/internal/api"
	"github.com/gopact-ai/9a/internal/authz"
	"github.com/gopact-ai/9a/internal/buildinfo"
	workspacepkg "github.com/gopact-ai/9a/internal/workspace"
	"github.com/spf13/cobra"
)

const (
	workspaceGroup   = "workspace"
	skillGroup       = "skills"
	integrationGroup = "integrations"
	accessGroup      = "access"
	executionGroup   = "execution"
	utilityGroup     = "utilities"
)

type cli struct {
	cwd    string
	call   func(api.Request) (json.RawMessage, error)
	getenv func(string) string
}

func newCLI(cwd string) *cli {
	return &cli{cwd: cwd, call: callRPC, getenv: os.Getenv}
}

func newRootCommand(c *cli) *cobra.Command {
	root := &cobra.Command{
		Use:   "9a",
		Short: "Expose external capabilities as local, agent-ready Skills",
		Long: `9a manages workspaces, declarative Skills, providers, permissions, and
capability invocations through the local ninead daemon.

Run "9a <command> --help" to see every positional argument, flag, and example
for a command. Help, completion, version, and validation do not require ninead.`,
		Example: `  9a attach
  9a search "weather forecast"
  printf '%s\n' '{"city":"Shanghai"}' | 9a invoke mcp/weather/forecast`,
		Version:                    buildinfo.Version,
		SilenceErrors:              true,
		SilenceUsage:               true,
		DisableAutoGenTag:          true,
		SuggestionsMinimumDistance: 2,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	root.SetVersionTemplate("9a {{.Version}}\n")
	root.AddGroup(
		&cobra.Group{ID: workspaceGroup, Title: "Workspace Commands:"},
		&cobra.Group{ID: skillGroup, Title: "Skill Commands:"},
		&cobra.Group{ID: integrationGroup, Title: "Integration Commands:"},
		&cobra.Group{ID: accessGroup, Title: "Access Commands:"},
		&cobra.Group{ID: executionGroup, Title: "Execution Commands:"},
		&cobra.Group{ID: utilityGroup, Title: "Utility Commands:"},
	)
	root.SetHelpCommandGroupID(utilityGroup)
	root.SetCompletionCommandGroupID(utilityGroup)
	root.AddCommand(
		c.newAttachCommand(),
		c.newStatusCommand(),
		c.newUpdateCommand(),
		c.newDetachCommand(),
		c.newValidateCommand(),
		c.newDeclarativeFileCommand(
			"add",
			"Add or replace a declarative Skill",
			"Validate a declarative Skill source, persist it, and reconcile its managed Skill in the current workspace.",
			"  9a add examples/declarative/open-meteo.yaml",
			true,
		),
		c.newDeclarativeFileCommand(
			"diff",
			"Preview declarative Skill changes",
			"Compare a declarative Skill source with the persisted version without changing daemon state.",
			"  9a diff examples/declarative/open-meteo.yaml",
			false,
		),
		c.newRemoveCommand(),
		c.newAdaptersCommand(),
		c.newProvidersCommand(),
		c.newACLCommand(),
		c.newTokensCommand(),
		c.newSearchCommand(),
		c.newProjectCommand(),
		c.newInvokeCommand(),
		c.newCallsCommand(),
		newCompletionCommand(),
		newVersionCommand(),
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

func helpOnly(cmd *cobra.Command, _ []string) error {
	return cmd.Help()
}

func (c *cli) runRequest(cmd *cobra.Command, request api.Request, plainString bool, autoAttachRoot string) error {
	if autoAttachRoot != "" && c.getenv("NINEA_AUTO_ATTACH") != "0" {
		if _, err := c.call(api.Request{Action: "workspace.attach", Root: autoAttachRoot, Backend: "auto"}); err != nil {
			return fmt.Errorf("auto-attach workspace %q: %w", autoAttachRoot, err)
		}
	}
	data, err := c.call(request)
	if err != nil {
		return err
	}
	if plainString {
		var value string
		if err := json.Unmarshal(data, &value); err != nil {
			return fmt.Errorf("decode daemon response: %w", err)
		}
		_, err := fmt.Fprintln(cmd.OutOrStdout(), value)
		return err
	}
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	if _, err := cmd.OutOrStdout().Write(data); err != nil {
		return err
	}
	_, err = cmd.OutOrStdout().Write([]byte("\n"))
	return err
}

func (c *cli) runRequestInCurrentWorkspace(cmd *cobra.Command, request api.Request, plainString bool) error {
	root, err := workspacepkg.Resolve("", c.cwd)
	if err != nil {
		return err
	}
	return c.runRequest(cmd, request, plainString, root)
}

func (c *cli) newAttachCommand() *cobra.Command {
	var workspace, backend string
	cmd := &cobra.Command{
		Use:     "attach",
		Short:   "Attach a workspace",
		GroupID: workspaceGroup,
		Long: `Attach the selected workspace and create NineA-managed Skill views.

The workspace defaults to the nearest project root from the current directory.
The auto backend prefers FUSE and reports a directory fallback in status.`,
		Example: `  9a attach
  9a attach --workspace /work/project --backend directory`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch backend {
			case "auto", "fuse", "directory":
			default:
				return fmt.Errorf("invalid --backend %q: expected auto, fuse, or directory", backend)
			}
			request, err := workspaceCommandRequest("attach", workspace, backend, c.cwd, false, false)
			if err != nil {
				return err
			}
			return c.runRequest(cmd, request, false, "")
		},
	}
	cmd.Flags().StringVar(&workspace, "workspace", "", "Workspace directory (default: discover from current directory)")
	cmd.Flags().StringVar(&backend, "backend", "auto", "Projection backend: auto, fuse, or directory")
	_ = cmd.MarkFlagDirname("workspace")
	_ = cmd.RegisterFlagCompletionFunc("backend", cobra.FixedCompletions(
		[]cobra.Completion{"auto", "fuse", "directory"},
		cobra.ShellCompDirectiveNoFileComp,
	))
	return cmd
}

func (c *cli) newStatusCommand() *cobra.Command {
	var workspace string
	cmd := &cobra.Command{
		Use:     "status",
		Short:   "Show workspace status",
		GroupID: workspaceGroup,
		Long: `Show the selected workspace's backend, fallback reason, and managed Skills.

Output is JSON. --json is retained so scripts can state the output contract explicitly.`,
		Example: `  9a status
  9a status --workspace /work/project --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			request, err := workspaceCommandRequest("status", workspace, "auto", c.cwd, false, false)
			if err != nil {
				return err
			}
			return c.runRequest(cmd, request, false, "")
		},
	}
	cmd.Flags().StringVar(&workspace, "workspace", "", "Workspace directory (default: discover from current directory)")
	cmd.Flags().Bool("json", false, "Print machine-readable JSON (currently the default)")
	_ = cmd.MarkFlagDirname("workspace")
	return cmd
}

func (c *cli) newUpdateCommand() *cobra.Command {
	var workspace string
	var check, all bool
	cmd := &cobra.Command{
		Use:     "update",
		Short:   "Update managed Skills",
		GroupID: workspaceGroup,
		Long: `Rediscover providers and reconcile managed Skills in the selected workspace.

Use --check for a read-only preview. Use --all to reconcile every attached
workspace instead of only the selected workspace. The two flags are mutually
exclusive.`,
		Example: `  9a update --check
  9a update
  9a update --all`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			request, err := workspaceCommandRequest("update", workspace, "auto", c.cwd, check, all)
			if err != nil {
				return err
			}
			autoAttachRoot := ""
			if !check && !all {
				autoAttachRoot = request.Root
			}
			return c.runRequest(cmd, request, false, autoAttachRoot)
		},
	}
	cmd.Flags().StringVar(&workspace, "workspace", "", "Workspace directory (default: discover from current directory)")
	cmd.Flags().BoolVar(&check, "check", false, "Preview changes without modifying the workspace")
	cmd.Flags().BoolVar(&all, "all", false, "Update every attached workspace")
	cmd.MarkFlagsMutuallyExclusive("check", "all")
	_ = cmd.MarkFlagDirname("workspace")
	return cmd
}

func (c *cli) newDetachCommand() *cobra.Command {
	var workspace string
	cmd := &cobra.Command{
		Use:     "detach",
		Short:   "Detach a workspace",
		GroupID: workspaceGroup,
		Long:    "Remove only NineA-managed views from the selected workspace. Persisted sources and providers are not deleted.",
		Example: `  9a detach
  9a detach --workspace /work/project`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			request, err := workspaceCommandRequest("detach", workspace, "auto", c.cwd, false, false)
			if err != nil {
				return err
			}
			return c.runRequest(cmd, request, false, "")
		},
	}
	cmd.Flags().StringVar(&workspace, "workspace", "", "Workspace directory (default: discover from current directory)")
	_ = cmd.MarkFlagDirname("workspace")
	return cmd
}

func (c *cli) newValidateCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "validate <source.yaml>",
		Short:   "Validate a declarative Skill file",
		GroupID: skillGroup,
		Long: `Strictly parse a declarative Skill source and print its name, digest, and
capability IDs as JSON. This command does not contact the daemon or change state.`,
		Example: "  9a validate examples/declarative/open-meteo.yaml",
		Args:    exactArgs("<source.yaml>", 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := validateDeclarativeFile(args[0])
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
		},
	}
}

func (c *cli) newDeclarativeFileCommand(name, short, long, example string, autoAttach bool) *cobra.Command {
	return &cobra.Command{
		Use:     name + " <source.yaml>",
		Short:   short,
		GroupID: skillGroup,
		Long:    long,
		Example: example,
		Args:    exactArgs("<source.yaml>", 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			request, err := declarativeFileRequest(name, args[0], c.cwd)
			if err != nil {
				return err
			}
			if autoAttach {
				return c.runRequestInCurrentWorkspace(cmd, request, false)
			}
			return c.runRequest(cmd, request, false, "")
		},
	}
}

func (c *cli) newRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "remove <skill-name>",
		Short:   "Remove a declarative Skill",
		GroupID: skillGroup,
		Long:    "Remove a persisted declarative source and only the managed Skill owned by that source.",
		Example: "  9a remove weather",
		Args:    exactArgs("<skill-name>", 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			request := declarativeRemoveRequest(args[0])
			return c.runRequest(cmd, request, false, "")
		},
	}
}

func (c *cli) newAdaptersCommand() *cobra.Command {
	parent := &cobra.Command{
		Use:     "adapters",
		Short:   "Manage executable adapters",
		GroupID: integrationGroup,
		Long:    "Register reviewed executables that implement the language-neutral 9a.adapter/v1 protocol.",
		RunE:    helpOnly,
	}
	parent.AddCommand(&cobra.Command{
		Use:     "add <protocol> <absolute-executable>",
		Short:   "Register an executable adapter",
		Long:    "Register an absolute executable path for a protocol, then make it available to provider discovery.",
		Example: "  9a adapters add billing /opt/ninea/billing-adapter",
		Args:    exactArgs("<protocol> <absolute-executable>", 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			canonical, err := adapterreg.ValidateRegistration(args[0], args[1])
			if err != nil {
				return fmt.Errorf("invalid adapter %q: %w", args[1], err)
			}
			request := adapterAddRequest(args[0], canonical)
			return c.runRequestInCurrentWorkspace(cmd, request, false)
		},
	})
	return parent
}

func (c *cli) newProvidersCommand() *cobra.Command {
	parent := &cobra.Command{
		Use:     "providers",
		Short:   "Manage capability providers",
		GroupID: integrationGroup,
		Long:    "Discover, persist, or remove providers for MCP, A2A, and registered executable adapter protocols.",
		RunE:    helpOnly,
	}
	parent.AddCommand(
		&cobra.Command{
			Use:   "add <protocol> <name> <endpoint>",
			Short: "Discover and add a provider",
			Long:  "Discover capabilities from an endpoint and persist the provider under its protocol and name.",
			Example: `  9a providers add mcp weather "stdio:/opt/bin/weather-server"
  9a providers add a2a research https://agent.example.com`,
			Args: exactArgs("<protocol> <name> <endpoint>", 3),
			RunE: func(cmd *cobra.Command, args []string) error {
				request := api.Request{Action: "provider.add", Protocol: args[0], Name: args[1], Endpoint: args[2]}
				return c.runRequestInCurrentWorkspace(cmd, request, false)
			},
		},
		&cobra.Command{
			Use:     "remove <protocol> <name>",
			Short:   "Remove a provider",
			Long:    "Remove a provider and the managed views generated from its capabilities.",
			Example: "  9a providers remove mcp weather",
			Args:    exactArgs("<protocol> <name>", 2),
			RunE: func(cmd *cobra.Command, args []string) error {
				request := api.Request{Action: "provider.remove", Protocol: args[0], Name: args[1]}
				return c.runRequest(cmd, request, false, "")
			},
		},
	)
	return parent
}

func (c *cli) newACLCommand() *cobra.Command {
	parent := &cobra.Command{
		Use:     "acl",
		Short:   "Manage capability permissions",
		GroupID: accessGroup,
		RunE:    helpOnly,
	}
	parent.AddCommand(&cobra.Command{
		Use:   "grant <identity> <capability> <permissions>",
		Short: "Grant capability permissions",
		Long: `Grant one or more comma-separated permissions to an identity for one capability.

Supported permissions are read, invoke, write, and admin.`,
		Example: "  9a acl grant support-agent api/orders/get-order read,invoke",
		Args:    exactArgs("<identity> <capability> <permissions>", 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(args[0]) == "" {
				return fmt.Errorf("<identity> must be non-empty")
			}
			if strings.TrimSpace(args[1]) == "" {
				return fmt.Errorf("<capability> must be non-empty")
			}
			permissions := strings.Split(args[2], ",")
			for i := range permissions {
				permissions[i] = strings.TrimSpace(permissions[i])
				permission, err := authz.ParsePermission(permissions[i])
				if err != nil {
					return err
				}
				permissions[i] = string(permission)
			}
			request := api.Request{Action: "acl.grant", Identity: args[0], Capability: args[1], Permissions: permissions}
			return c.runRequest(cmd, request, false, "")
		},
	})
	return parent
}

func (c *cli) newTokensCommand() *cobra.Command {
	parent := &cobra.Command{
		Use:     "tokens",
		Short:   "Manage identity tokens",
		GroupID: accessGroup,
		RunE:    helpOnly,
	}
	parent.AddCommand(&cobra.Command{
		Use:     "create <identity>",
		Short:   "Create an identity token",
		Long:    "Create a bearer token for an identity and print only the token to stdout.",
		Example: `  export NINEA_TOKEN="$(9a tokens create support-agent)"`,
		Args:    exactArgs("<identity>", 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return c.runRequest(cmd, api.Request{Action: "token.create", Identity: args[0]}, true, "")
		},
	})
	return parent
}

func (c *cli) newSearchCommand() *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:     "search <query...>",
		Short:   "Search visible capabilities",
		GroupID: executionGroup,
		Long:    "Search capabilities visible to the current identity and print matching Catalog entries in the selected format.",
		Example: `  9a search "weather temperature" --format json
  9a search weather temperature`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return fmt.Errorf("%s requires at least one search term: <query...>", cmd.CommandPath())
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if format != "json" {
				return fmt.Errorf("unsupported --format %q: expected json", format)
			}
			request := api.Request{Action: "search", Query: strings.Join(args, " "), Format: format}
			return c.runRequestInCurrentWorkspace(cmd, request, false)
		},
	}
	cmd.Flags().StringVar(&format, "format", "json", "Output format: json")
	_ = cmd.RegisterFlagCompletionFunc("format", cobra.FixedCompletions(
		[]cobra.Completion{"json"},
		cobra.ShellCompDirectiveNoFileComp,
	))
	return cmd
}

func (c *cli) newProjectCommand() *cobra.Command {
	parent := &cobra.Command{
		Use:     "project",
		Short:   "Project capabilities as filesystem Skills",
		GroupID: executionGroup,
		RunE:    helpOnly,
	}
	parent.AddCommand(&cobra.Command{
		Use:     "add <capability> <skills-root>",
		Short:   "Project one capability into a Skills directory",
		Long:    "Materialize one visible capability as a managed filesystem Skill under the selected Skills root.",
		Example: "  9a project add mcp/weather/get-weather .agents/skills",
		Args:    exactArgs("<capability> <skills-root>", 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := filepath.Abs(args[1])
			if err != nil {
				return err
			}
			workspaceRoot, err := workspacepkg.Resolve("", c.cwd)
			if err != nil {
				return err
			}
			if c.getenv("NINEA_AUTO_ATTACH") == "0" {
				workspaceRoot = workspaceForProjectionRoot(root)
			}
			request := api.Request{Action: "project.add", Capability: args[0], Workspace: workspaceRoot, Root: root}
			return c.runRequest(cmd, request, false, workspaceRoot)
		},
	})
	return parent
}

func (c *cli) newInvokeCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "invoke <capability>",
		Short:   "Invoke a capability synchronously",
		GroupID: executionGroup,
		Long: `Read JSON from stdin, invoke a capability, and print its JSON result.

Empty stdin is treated as {}. The CLI waits up to 30 seconds; use "9a calls
start" for work that must continue after the client exits.`,
		Example: `  printf '%s\n' '{"city":"Shanghai"}' | 9a invoke mcp/weather/forecast`,
		Args:    exactArgs("<capability>", 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			request, err := invokeRequest(args[0], cmd.InOrStdin())
			if err != nil {
				return err
			}
			return c.runRequest(cmd, request, false, "")
		},
	}
}

func (c *cli) newCallsCommand() *cobra.Command {
	parent := &cobra.Command{
		Use:     "calls",
		Short:   "Manage asynchronous calls",
		GroupID: executionGroup,
		Long:    "Start long-running capability calls, inspect persistent state and events, or request cancellation.",
		RunE:    helpOnly,
	}
	parent.AddCommand(
		c.newCallsStartCommand(),
		c.newCallsGetCommand(),
		c.newCallsEventsCommand(),
		c.newCallsCancelCommand(),
	)
	return parent
}

func (c *cli) newCallsStartCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "start <capability>",
		Short:   "Start an asynchronous call",
		Long:    "Read JSON from stdin, persist the call, and print only its call ID to stdout.",
		Example: `  CALL_ID="$(printf '%s\n' '{"city":"Shanghai"}' | 9a calls start mcp/weather/forecast)"`,
		Args:    exactArgs("<capability>", 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			request, plain, err := callsRequest("start", args[0], cmd.InOrStdin(), 0, 0)
			if err != nil {
				return err
			}
			return c.runRequest(cmd, request, plain, "")
		},
	}
}

func (c *cli) newCallsGetCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "get <call-id>",
		Short:   "Get asynchronous call state",
		Long:    "Print the persistent state and terminal result for one call as JSON.",
		Example: "  9a calls get call_01HXYZ",
		Args:    exactArgs("<call-id>", 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			request, plain, err := callsRequest("get", args[0], cmd.InOrStdin(), 0, 0)
			if err != nil {
				return err
			}
			return c.runRequest(cmd, request, plain, "")
		},
	}
}

func (c *cli) newCallsEventsCommand() *cobra.Command {
	var after, limit int
	cmd := &cobra.Command{
		Use:   "events <call-id>",
		Short: "List asynchronous call events",
		Long: `Print one persistent event page as JSON. --after is an exclusive sequence
cursor; --limit bounds the number of returned events.`,
		Example: "  9a calls events call_01HXYZ --after 100 --limit 25",
		Args:    exactArgs("<call-id>", 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if after < 0 {
				return fmt.Errorf("--after must be zero or greater")
			}
			if cmd.Flags().Changed("limit") && limit <= 0 {
				return fmt.Errorf("--limit must be greater than zero")
			}
			request, plain, err := callsRequest("events", args[0], cmd.InOrStdin(), after, limit)
			if err != nil {
				return err
			}
			return c.runRequest(cmd, request, plain, "")
		},
	}
	cmd.Flags().IntVar(&after, "after", 0, "Return events after this sequence number (minimum 0)")
	cmd.Flags().IntVar(&limit, "limit", 0, "Maximum events to return (server default when omitted)")
	return cmd
}

func (c *cli) newCallsCancelCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "cancel <call-id>",
		Short:   "Cancel an active asynchronous call",
		Long:    "Request cancellation of an active call and wait for adapter confirmation.",
		Example: "  9a calls cancel call_01HXYZ",
		Args:    exactArgs("<call-id>", 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			request, plain, err := callsRequest("cancel", args[0], cmd.InOrStdin(), 0, 0)
			if err != nil {
				return err
			}
			return c.runRequest(cmd, request, plain, "")
		},
	}
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
		Short:   "Print the client version",
		GroupID: utilityGroup,
		Long:    "Print the embedded 9a client version. Published binaries receive this value from the release tag.",
		Example: "  9a version",
		Args:    cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "9a %s\n", buildinfo.Version)
		},
	}
}
