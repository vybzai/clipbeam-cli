package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// globalFlags holds the output-discipline flags every command honors (PLAN §8.1):
// stdout=data, stderr=diagnostics; --json wraps output and implies --no-color;
// --quiet silences stderr; --verbose adds a stderr trace. CLIPBEAM_JSON=1 flips
// every command to --json (and watch to NDJSON).
type globalFlags struct {
	json    bool
	noColor bool
	quiet   bool
	verbose bool
}

var gflags globalFlags

// Execute builds the command tree and runs it, returning the process exit code per
// the unified table (PLAN §8.3). The version string is injected from main and stored
// in cmdVersion so version/schema/health surface it.
func Execute(version string) int {
	cmdVersion = version
	root := newRootCmd(version)
	if err := root.Execute(); err != nil {
		return exitCodeFor(err)
	}
	return ExitOK
}

// exitCodeFor maps an error to the unified exit-code table (PLAN §8.3) and emits the
// failure diagnostic. A codedError carries its own exit code (decided at the failure
// site, where the HTTP/SSH/local provenance is known). cobra's own argument/flag
// validation errors map to ExitUsage (the shim's `exit 2`). Everything else is a
// generic failure (1). Under --json the structured error envelope is written to
// stdout (the data sink); the human message always goes to stderr.
func exitCodeFor(err error) int {
	if err == nil {
		return ExitOK
	}
	code := ExitGeneric
	var ce *codedError
	if errors.As(err, &ce) {
		code = ce.code
	} else if isUsageError(err) {
		code = ExitUsage
	}
	if gflags.json {
		// stdout = data: emit the §8.2 error envelope as the deliverable.
		(out{stdout: os.Stdout, stderr: os.Stderr, json: true}).emitJSONError(err.Error(), code)
	}
	// Diagnostics always go to stderr (suppressed under --quiet).
	if !gflags.quiet {
		fmt.Fprintln(os.Stderr, "clipbeam:", err)
	}
	return code
}

// isUsageError reports whether err is a cobra argument/flag validation error, which
// maps to ExitUsage (PLAN §8.3). cobra reports these as plain errors with a stable
// message prefix; we match those prefixes so a bad invocation exits 2, not 1.
func isUsageError(err error) bool {
	msg := err.Error()
	for _, p := range []string{
		"unknown command",
		"unknown flag",
		"unknown shorthand flag",
		"accepts ",
		"requires ",
		"invalid argument",
		"flag needs an argument",
		"required flag",
	} {
		if strings.HasPrefix(msg, p) || strings.Contains(msg, p) {
			return true
		}
	}
	return false
}

// newRootCmd assembles the cobra root with global flags and every verb subcommand.
func newRootCmd(version string) *cobra.Command {
	root := &cobra.Command{
		Use:   "clipbeam",
		Short: "Beam files, screenshots, and clipboard straight into your remote agent's reach",
		Long: "clipbeam beams files, screenshots, clipboard content, and agent messages " +
			"over SSH or Tailscale into a headless box you are already SSH'd into — landing " +
			"each payload on the box's disk and printing its absolute path. No cloud, no account.\n\n" +
			"Built for coding agents: every command is --json-able and self-describing " +
			"(see `clipbeam schema`).",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true, // Execute prints the error + maps the exit code
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}

	pf := root.PersistentFlags()
	pf.BoolVar(&gflags.json, "json", envFlag("CLIPBEAM_JSON"), "emit a versioned JSON object on stdout (implies --no-color)")
	pf.BoolVar(&gflags.noColor, "no-color", false, "disable ANSI color (also auto-disabled off-TTY and under NO_COLOR)")
	pf.BoolVar(&gflags.quiet, "quiet", false, "silence stderr diagnostics")
	pf.BoolVar(&gflags.verbose, "verbose", false, "add a stderr trace")

	root.AddCommand(
		newSendCmd(),
		newShotCmd(),
		newMsgCmd(),
		newPushCmd(),
		newLastCmd(),
		newWaitCmd(),
		newRecvCmd(),
		newWatchCmd(),
		newHealthCmd(),
		newSchemaCmd(),
		newVersionCmd(version),
		newInstallSkillCmd(),
		newServeCmd(),
		newIngestCmd(),
		newSetupCmd(),
		newDoctorCmd(),
		newServiceCmd(),
	)

	// `help --json` is an exact alias for `schema` (PLAN §8.5): a runtime
	// self-description document. Plain help / -h / no-args prints the human usage table.
	// We override cobra's help function so a --json help request emits the schema doc to
	// stdout (the data sink) instead of the usage text.
	defaultHelp := root.HelpFunc()
	root.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		if gflags.json {
			_ = runSchema(out{stdout: cmd.OutOrStdout(), stderr: cmd.ErrOrStderr(), json: true})
			return
		}
		defaultHelp(cmd, args)
	})

	return root
}

// envFlag reports whether an env var is set to a truthy non-empty value (used so
// CLIPBEAM_JSON=1 defaults --json on, PLAN §8.1).
func envFlag(name string) bool {
	v := os.Getenv(name)
	return v != "" && v != "0" && v != "false"
}
