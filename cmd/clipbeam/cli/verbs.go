package cli

import "github.com/spf13/cobra"

// This file defines the cobra constructor for every verb (PLAN §8). Each builds a
// command with the correct Use/Short/args + flag surface and wires its RunE to the
// implementation in the verbs_*.go files. Flag variables are command-scoped (closed
// over by RunE) so the surface and its handler stay co-located.

// --- send (PLAN §8.7): file/shot default to clipboard channel; --agent flips ---

func newSendCmd() *cobra.Command {
	var agent bool
	cmd := &cobra.Command{
		Use:   "send <file> [target]",
		Short: "Beam a file to the box; prints the remote absolute path on stdout",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSend(newOut(cmd), args[0], targetArg(args, 1), agent)
		},
	}
	cmd.Flags().BoolVar(&agent, "agent", false, "send on the agent channel instead of clipboard")
	return cmd
}

// --- shot (PLAN §8.7): screenshot → box, prints remote path ---

func newShotCmd() *cobra.Command {
	var agent bool
	cmd := &cobra.Command{
		Use:   "shot [target]",
		Short: "Capture a screenshot, beam it to the box, print the remote path",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runShot(newOut(cmd), targetArg(args, 0), agent)
		},
	}
	cmd.Flags().BoolVar(&agent, "agent", false, "send on the agent channel instead of clipboard")
	return cmd
}

// --- msg (PLAN §8.7): always the agent channel (X-ClipBeam-Channel: agent) ---

func newMsgCmd() *cobra.Command {
	var replyTo string
	cmd := &cobra.Command{
		Use:   "msg <text> [target]",
		Short: "Beam a text message on the agent channel",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMsg(newOut(cmd), args[0], targetArg(args, 1), replyTo)
		},
	}
	cmd.Flags().StringVar(&replyTo, "reply-to", "", "prepend an opt-in [clipbeam:cid=<uuid>] reply token (agent channel only)")
	return cmd
}

// --- push (PLAN §8.7): bare POST /push, reads local clipboard ---

func newPushCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "push [target]",
		Short: "Beam the local clipboard contents to the box",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPush(newOut(cmd), targetArg(args, 0))
		},
	}
}

// --- last (PLAN §8.1): bare absolute path, NO trailing newline ---

func newLastCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "last",
		Short: "Print the most-recently-received absolute path (no trailing newline)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLast(newOut(cmd))
		},
	}
}

// --- wait (PLAN §8.1): fixed-120 s long-poll → bare path, no trailing newline ---

func newWaitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "wait",
		Short: "Block up to 120s for the next received path; print it (no trailing newline)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWait(newOut(cmd))
		},
	}
}

// --- recv (PLAN §8.2): dequeue one agent FIFO item; labeled body or --json ---

func newRecvCmd() *cobra.Command {
	var timeout int
	cmd := &cobra.Command{
		Use:   "recv",
		Short: "Dequeue one agent-channel item (default 120s long-poll)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if timeout < 0 {
				return usageError("recv: --timeout must be >= 0 (got %d)", timeout)
			}
			return runRecv(newOut(cmd), timeout)
		},
	}
	cmd.Flags().IntVar(&timeout, "timeout", 120, "long-poll seconds (0 = a blocking poll seeing nothing is normal)")
	return cmd
}

// --- watch (PLAN §8.4): NDJSON stream over the long-poll endpoints ---

func newWatchCmd() *cobra.Command {
	var channel string
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Stream received items (NDJSON under --json) until SIGINT",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWatch(newOut(cmd), channel)
		},
	}
	cmd.Flags().StringVar(&channel, "channel", "both", "agent | clipboard | both")
	return cmd
}

// --- health (PLAN §8.2): frozen /health key names, CLI values ---

func newHealthCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "health [target]",
		Short: "Check a peer's /health (liveness via ok==true; values are node-specific)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHealth(newOut(cmd), targetArg(args, 0))
		},
	}
}

// --- schema (PLAN §8.5): runtime self-description document ---

func newSchemaCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "schema",
		Short: "Print the machine-readable self-description (verbs, flags, exit codes)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSchema(newOut(cmd))
		},
	}
}

// --- version (PLAN §8.2): net-new CLI surface; --json schema ---

func newVersionCmd(version string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the clipbeam version (use --json for the structured schema)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVersion(newOut(cmd))
		},
	}
}

// --- install-skill (PLAN §8.8): write the embedded SKILL.md/AGENTS.md ---

func newInstallSkillCmd() *cobra.Command {
	var target, dir string
	var force, listTargets bool
	cmd := &cobra.Command{
		Use:   "install-skill",
		Short: "Install the clipbeam agent skill (SKILL.md/AGENTS.md) for Claude/Codex",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInstallSkill(newOut(cmd), target, dir, force, listTargets)
		},
	}
	f := cmd.Flags()
	f.StringVar(&target, "target", "all", "claude | codex | all")
	f.StringVar(&dir, "dir", "", "override the skill output directory")
	f.BoolVar(&force, "force", false, "overwrite an existing skill file")
	f.BoolVar(&listTargets, "list-targets", false, "enumerate detected agent config dirs")
	return cmd
}

// --- serve (PLAN §5.7): the receiver daemon ---

func newServeCmd() *cobra.Command {
	var listen string
	var setClipboard bool
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the receiver daemon (unix socket by default; --listen tcp|tailscale)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServe(newOut(cmd), listen, setClipboard)
		},
	}
	f := cmd.Flags()
	f.StringVar(&listen, "listen", "socket", "socket | tcp | tailscale (tcp/tailscale require the token; see SECURITY.md)")
	f.BoolVar(&setClipboard, "set-clipboard", false, "best-effort set the X/Wayland clipboard on Linux (opt-in)")
	return cmd
}

// --- ingest (PLAN §5.1): remote-side CB01 reader, prints saved paths ---

func newIngestCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "ingest",
		Short:  "Read a CB01 frame from stdin, save it, print the saved abs path(s)",
		Args:   cobra.NoArgs,
		Hidden: true, // internal: the daemonless-exec remote endpoint
		RunE: func(cmd *cobra.Command, args []string) error {
			return runIngest(newOut(cmd))
		},
	}
}

// --- setup (PLAN §9.5): the keystone remote bootstrap ---

func newSetupCmd() *cobra.Command {
	var opts setupOptions
	cmd := &cobra.Command{
		Use:   "setup <user@host>",
		Short: "Bootstrap the box over SSH: stream the binary, pair, record the abs path",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSetup(newOut(cmd), args[0], opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.serve, "serve", "exec", "exec | socket | tcp | tailscale (default daemonless-exec)")
	f.BoolVar(&opts.acceptNew, "accept-new-hostkey", false, "record an unknown host key non-interactively (TOFU)")
	f.BoolVar(&opts.force, "force", false, "re-stream the binary even if the remote version matches")
	f.BoolVar(&opts.fromRelease, "from-release", false, "fetch the remote binary via a remote-side curl|sh")
	return cmd
}

// --- doctor (PLAN §4.2/§6.4): environment + secret-storage diagnostics ---

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose config, token storage, permissions, and transport readiness",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor(newOut(cmd))
		},
	}
}

// --- service (PLAN §9.7): optional off-by-default service units ---

func newServiceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service <install|uninstall>",
		Short: "Manage the optional systemd --user / launchd service unit (off by default)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runService(newOut(cmd), args[0])
		},
	}
	return cmd
}

// targetArg returns the positional target at index i, or "" when absent (a verb with
// no target uses the default alias, PLAN §5.5).
func targetArg(args []string, i int) string {
	if i < len(args) {
		return args[i]
	}
	return ""
}
