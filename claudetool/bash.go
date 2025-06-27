package claudetool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"sketch.dev/claudetool/bashkit"
	"sketch.dev/llm"
	"sketch.dev/llm/conversation"
)

// PermissionCallback is a function type for checking if a command is allowed to run
type PermissionCallback func(command string) error

// BashTool is a struct for executing shell commands with bash -c and optional timeout
type BashTool struct {
	// CheckPermission is called before running any command, if set
	CheckPermission PermissionCallback
	// EnableJITInstall enables just-in-time tool installation for missing commands
	EnableJITInstall bool
}

const (
	EnableBashToolJITInstall = true
	NoBashToolJITInstall     = false
)

// NewBashTool creates a new Bash tool with optional permission callback
func NewBashTool(checkPermission PermissionCallback, enableJITInstall bool) *llm.Tool {
	tool := &BashTool{
		CheckPermission:  checkPermission,
		EnableJITInstall: enableJITInstall,
	}

	return &llm.Tool{
		Name:        bashName,
		Description: strings.TrimSpace(bashDescription),
		InputSchema: llm.MustSchema(bashInputSchema),
		Run:         tool.Run,
	}
}

// The Bash tool executes shell commands with bash -c and optional timeout
var Bash = NewBashTool(nil, NoBashToolJITInstall)

const (
	bashName        = "bash"
	bashDescription = `
Executes a shell command using bash -c with an optional timeout, returning combined stdout and stderr.
When run with background flag, the process may keep running after the tool call returns, and
the agent can inspect the output by reading the output files. Use the background task when, for example,
starting a server to test something. Be sure to kill the process group when done.
`
	// If you modify this, update the termui template for prettier rendering.
	bashInputSchema = `
{
  "type": "object",
  "required": ["command"],
  "properties": {
    "command": {
      "type": "string",
      "description": "Shell script to execute"
    },
    "timeout": {
      "type": "string",
      "description": "Timeout as a Go duration string, defaults to 10s if background is false; 10m if background is true"
    },
    "background": {
      "type": "boolean",
      "description": "If true, executes the command in the background without waiting for completion"
    }
  }
}
`
)

type bashInput struct {
	Command    string `json:"command"`
	Timeout    string `json:"timeout,omitempty"`
	Background bool   `json:"background,omitempty"`
}

type BackgroundResult struct {
	PID        int    `json:"pid"`
	StdoutFile string `json:"stdout_file"`
	StderrFile string `json:"stderr_file"`
}

func (i *bashInput) timeout() time.Duration {
	if i.Timeout != "" {
		dur, err := time.ParseDuration(i.Timeout)
		if err == nil {
			return dur
		}
	}

	// Otherwise, use different defaults based on background mode
	if i.Background {
		return 10 * time.Minute
	} else {
		return 10 * time.Second
	}
}

func (b *BashTool) Run(ctx context.Context, m json.RawMessage) ([]llm.Content, error) {
	var req bashInput
	if err := json.Unmarshal(m, &req); err != nil {
		return nil, fmt.Errorf("failed to unmarshal bash command input: %w", err)
	}

	// do a quick permissions check (NOT a security barrier)
	err := bashkit.Check(req.Command)
	if err != nil {
		return nil, err
	}

	// Custom permission callback if set
	if b.CheckPermission != nil {
		if err := b.CheckPermission(req.Command); err != nil {
			return nil, err
		}
	}

	// Check for missing tools and try to install them if needed, best effort only
	if b.EnableJITInstall {
		err := b.checkAndInstallMissingTools(ctx, req.Command)
		if err != nil {
			slog.DebugContext(ctx, "failed to auto-install missing tools", "error", err)
		}
	}

	// If Background is set to true, use executeBackgroundBash
	if req.Background {
		result, err := executeBackgroundBash(ctx, req)
		if err != nil {
			return nil, err
		}
		// Marshal the result to JSON
		// TODO: emit XML(-ish) instead?
		output, err := json.Marshal(result)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal background result: %w", err)
		}
		return llm.TextContent(string(output)), nil
	}

	// For foreground commands, use executeBash
	out, execErr := executeBash(ctx, req)
	if execErr != nil {
		return nil, execErr
	}
	return llm.TextContent(out), nil
}

const maxBashOutputLength = 131072

// executeBash runs a bash command with a timeout, preferring interactive PTY execution and falling back to standard exec if PTY fails.
// It returns the combined output of stdout and stderr, or an error if execution fails.
func executeBash(ctx context.Context, req bashInput) (string, error) {
	execCtx, cancel := context.WithTimeout(ctx, req.timeout())
	defer cancel()

	// Try PTY first for better interactive support, fallback to exec if it fails
	if output, err := executeBashWithPty(execCtx, req); err == nil {
		return output, nil
	} else {
		// Log PTY failure for debugging but don't fail the command
		slog.Debug("PTY execution failed, falling back to exec", "error", err)
	}

	// Fallback to original exec-based implementation
	return executeBashWithExec(execCtx, req)
}

// executeBashWithPty runs a bash command using a pseudo-terminal (pty) to provide interactive shell support.
//
// The function starts a bash shell with a pty, writes the command followed by an exit directive, and reads all output from the pty. It cleans the output by removing shell prompts and command echoes. If the command exceeds the maximum allowed output length or times out, it returns an error with partial output. The process group is killed on timeout to ensure cleanup.
//
// Returns the cleaned command output, or an error if execution fails, output is too long, or the command times out.
func executeBashWithPty(ctx context.Context, req bashInput) (string, error) {
	// Start bash with a pty for better interactive support
	cmd := exec.CommandContext(ctx, "bash")
	cmd.Dir = WorkingDir(ctx)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Set environment with SKETCH=1 and TERM for proper pty behavior
	cmd.Env = append(os.Environ(), "SKETCH=1", "TERM=xterm-256color")

	// Start the command with a pty
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return "", fmt.Errorf("failed to start pty: %w", err)
	}
	defer ptmx.Close()

	proc := cmd.Process
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded && proc != nil {
				// Kill the entire process group.
				syscall.Kill(-proc.Pid, syscall.SIGKILL)
			}
		case <-done:
		}
	}()

	// Send the command to the pty followed by exit to ensure bash terminates
	cmdLine := req.Command + "; exit $?\n"
	_, err = ptmx.Write([]byte(cmdLine))
	if err != nil {
		return "", fmt.Errorf("failed to write command to pty: %w", err)
	}

	// Read all output from the pty
	var output bytes.Buffer
	_, err = io.Copy(&output, ptmx)
	if err != nil && err != io.EOF {
		// Don't treat EOF as an error since it's expected when the process exits
		slog.Debug("pty read error (may be normal)", "error", err)
	}

	// Wait for command to complete
	err = cmd.Wait()
	close(done)

	// Process the output - remove shell prompt and command echo if present
	outputStr := output.String()
	outputStr = cleanPtyOutput(outputStr, req.Command)

	longOutput := len(outputStr) > maxBashOutputLength
	var outstr string
	if longOutput {
		outstr = fmt.Sprintf("output too long: got %v, max is %v\ninitial bytes of output:\n%s",
			humanizeBytes(len(outputStr)), humanizeBytes(maxBashOutputLength),
			outputStr[:1024],
		)
	} else {
		outstr = outputStr
	}

	if ctx.Err() == context.DeadlineExceeded {
		// Get the partial output that was captured before the timeout
		partialOutput := outputStr
		// Truncate if the output is too large
		if len(partialOutput) > maxBashOutputLength {
			partialOutput = partialOutput[:maxBashOutputLength] + "\n[output truncated due to size]\n"
		}
		return "", fmt.Errorf("command timed out after %s\nCommand output (until it timed out):\n%s", req.timeout(), outstr)
	}
	if err != nil {
		return "", fmt.Errorf("command failed: %w\n%s", err, outstr)
	}

	if longOutput {
		return "", fmt.Errorf("%s", outstr)
	}

	return outputStr, nil
}

// executeBashWithExec runs a bash command using exec, capturing combined stdout and stderr output.
// The process group is set to allow for termination on timeout. If the output exceeds the maximum allowed length, or if the command fails or times out, an error is returned containing relevant output.
func executeBashWithExec(ctx context.Context, req bashInput) (string, error) {
	// Can't do the simple thing and call CombinedOutput because of the need to kill the process group.
	cmd := exec.CommandContext(ctx, "bash", "-c", req.Command)
	cmd.Dir = WorkingDir(ctx)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Set environment with SKETCH=1
	cmd.Env = append(os.Environ(), "SKETCH=1")

	var output bytes.Buffer
	cmd.Stdin = nil
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("command failed: %w", err)
	}
	proc := cmd.Process
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded && proc != nil {
				// Kill the entire process group.
				syscall.Kill(-proc.Pid, syscall.SIGKILL)
			}
		case <-done:
		}
	}()

	err := cmd.Wait()
	close(done)

	longOutput := output.Len() > maxBashOutputLength
	var outstr string
	if longOutput {
		outstr = fmt.Sprintf("output too long: got %v, max is %v\ninitial bytes of output:\n%s",
			humanizeBytes(output.Len()), humanizeBytes(maxBashOutputLength),
			output.Bytes()[:1024],
		)
	} else {
		outstr = output.String()
	}

	if ctx.Err() == context.DeadlineExceeded {
		// Get the partial output that was captured before the timeout
		partialOutput := output.String()
		// Truncate if the output is too large
		if len(partialOutput) > maxBashOutputLength {
			partialOutput = partialOutput[:maxBashOutputLength] + "\n[output truncated due to size]\n"
		}
		return "", fmt.Errorf("command timed out after %s\nCommand output (until it timed out):\n%s", req.timeout(), outstr)
	}
	if err != nil {
		return "", fmt.Errorf("command failed: %w\n%s", err, outstr)
	}

	if longOutput {
		return "", fmt.Errorf("%s", outstr)
	}

	return output.String(), nil
}

func humanizeBytes(bytes int) string {
	switch {
	case bytes < 4*1024:
		return fmt.Sprintf("%dB", bytes)
	case bytes < 1024*1024:
		kb := int(math.Round(float64(bytes) / 1024.0))
		return fmt.Sprintf("%dkB", kb)
	case bytes < 1024*1024*1024:
		mb := int(math.Round(float64(bytes) / (1024.0 * 1024.0)))
		return fmt.Sprintf("%dMB", mb)
	}
	return "more than 1GB"
}

// executeBackgroundBash runs a shell command in the background, returning the process ID and paths to output files.
// It first attempts to use a pseudo-terminal (PTY) for interactive support, falling back to a standard exec-based method if PTY is unavailable.
func executeBackgroundBash(ctx context.Context, req bashInput) (*BackgroundResult, error) {
	// Try PTY first for better interactive support, fallback to exec if it fails
	if result, err := executeBackgroundBashWithPty(ctx, req); err == nil {
		return result, nil
	} else {
		// Log PTY failure for debugging but don't fail the command
		slog.Debug("Background PTY execution failed, falling back to exec", "error", err)
	}

	// Fallback to original exec-based implementation
	return executeBackgroundBashWithExec(ctx, req)
}

// executeBackgroundBashWithPty runs a shell command in the background using a pseudo-terminal (pty), capturing combined stdout and stderr output to a file.
// It returns the process ID and paths to the output files for later retrieval. If a timeout is specified, the process group is forcibly killed after the duration.
func executeBackgroundBashWithPty(ctx context.Context, req bashInput) (*BackgroundResult, error) {
	// Create temporary directory for output files
	tmpDir, err := os.MkdirTemp("", "sketch-bg-")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp directory: %w", err)
	}

	// Create temp files for stdout and stderr (with pty, both go to same output)
	stdoutFile := filepath.Join(tmpDir, "stdout")
	stderrFile := filepath.Join(tmpDir, "stderr")

	// Prepare the command
	cmd := exec.Command("bash")
	cmd.Dir = WorkingDir(ctx)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Set environment with SKETCH=1 and TERM for proper pty behavior
	cmd.Env = append(os.Environ(), "SKETCH=1", "TERM=xterm-256color")

	// Start the command with a pty
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to start background pty: %w", err)
	}

	// Open output files
	stdout, err := os.Create(stdoutFile)
	if err != nil {
		ptmx.Close()
		return nil, fmt.Errorf("failed to create stdout file: %w", err)
	}

	// For background tasks, we create an empty stderr file to maintain API compatibility
	// but all output (stdout+stderr) goes to stdout file since pty combines them
	_, err = os.Create(stderrFile)
	if err != nil {
		stdout.Close()
		ptmx.Close()
		return nil, fmt.Errorf("failed to create stderr file: %w", err)
	}

	// Send the command to the pty
	cmdLine := req.Command + "\n"
	_, err = ptmx.Write([]byte(cmdLine))
	if err != nil {
		stdout.Close()
		ptmx.Close()
		return nil, fmt.Errorf("failed to write command to background pty: %w", err)
	}

	// Start a goroutine to copy pty output to the stdout file
	go func() {
		defer stdout.Close()
		defer ptmx.Close()

		// Copy all pty output to stdout file
		io.Copy(stdout, ptmx)

		// Wait for process to complete (reap the process)
		cmd.Wait()
	}()

	// Set up timeout handling if a timeout was specified
	pid := cmd.Process.Pid
	timeout := req.timeout()
	if timeout > 0 {
		// Launch a goroutine that will kill the process after the timeout
		go func() {
			// TODO(josh): this should use a context instead of a sleep, like executeBash above,
			// to avoid goroutine leaks. Possibly should be partially unified with executeBash.
			// Sleep for the timeout duration
			time.Sleep(timeout)

			// TODO(philip): Should we do SIGQUIT and then SIGKILL in 5s?

			// Try to kill the process group
			killErr := syscall.Kill(-pid, syscall.SIGKILL)
			if killErr != nil {
				// If killing the process group fails, try to kill just the process
				syscall.Kill(pid, syscall.SIGKILL)
			}
		}()
	}

	// Return the process ID and file paths
	return &BackgroundResult{
		PID:        cmd.Process.Pid,
		StdoutFile: stdoutFile,
		StderrFile: stderrFile,
	}, nil
}

// executeBackgroundBashWithExec runs a bash command in the background using exec, redirecting stdout and stderr to temporary files.
// It returns a BackgroundResult containing the process ID and paths to the output files. If a timeout is specified, the process group is killed after the timeout expires.
func executeBackgroundBashWithExec(ctx context.Context, req bashInput) (*BackgroundResult, error) {
	// Create temporary directory for output files
	tmpDir, err := os.MkdirTemp("", "sketch-bg-")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp directory: %w", err)
	}

	// Create temp files for stdout and stderr
	stdoutFile := filepath.Join(tmpDir, "stdout")
	stderrFile := filepath.Join(tmpDir, "stderr")

	// Prepare the command
	cmd := exec.Command("bash", "-c", req.Command)
	cmd.Dir = WorkingDir(ctx)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Set environment with SKETCH=1
	cmd.Env = append(os.Environ(), "SKETCH=1")

	// Open output files
	stdout, err := os.Create(stdoutFile)
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout file: %w", err)
	}
	defer stdout.Close()

	stderr, err := os.Create(stderrFile)
	if err != nil {
		return nil, fmt.Errorf("failed to create stderr file: %w", err)
	}
	defer stderr.Close()

	// Configure command to use the files
	cmd.Stdin = nil
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	// Start the command
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start background command: %w", err)
	}

	// Start a goroutine to reap the process when it finishes
	go func() {
		cmd.Wait()
		// Process has been reaped
	}()

	// Set up timeout handling if a timeout was specified
	pid := cmd.Process.Pid
	timeout := req.timeout()
	if timeout > 0 {
		// Launch a goroutine that will kill the process after the timeout
		go func() {
			// TODO(josh): this should use a context instead of a sleep, like executeBash above,
			// to avoid goroutine leaks. Possibly should be partially unified with executeBash.
			// Sleep for the timeout duration
			time.Sleep(timeout)

			// TODO(philip): Should we do SIGQUIT and then SIGKILL in 5s?

			// Try to kill the process group
			killErr := syscall.Kill(-pid, syscall.SIGKILL)
			if killErr != nil {
				// If killing the process group fails, try to kill just the process
				syscall.Kill(pid, syscall.SIGKILL)
			}
		}()
	}

	// Return the process ID and file paths
	return &BackgroundResult{
		PID:        cmd.Process.Pid,
		StdoutFile: stdoutFile,
		StderrFile: stderrFile,
	}, nil
}

// checkAndInstallMissingTools analyzes a bash command and attempts to automatically install any missing tools.
func (b *BashTool) checkAndInstallMissingTools(ctx context.Context, command string) error {
	commands, err := bashkit.ExtractCommands(command)
	if err != nil {
		return err
	}

	autoInstallMu.Lock()
	defer autoInstallMu.Unlock()

	var missing []string
	for _, cmd := range commands {
		if doNotAttemptToolInstall[cmd] {
			continue
		}
		_, err := exec.LookPath(cmd)
		if err == nil {
			doNotAttemptToolInstall[cmd] = true // spare future LookPath calls
			continue
		}
		missing = append(missing, cmd)
	}

	if len(missing) == 0 {
		return nil
	}

	err = b.installTools(ctx, missing)
	if err != nil {
		return err
	}
	for _, cmd := range missing {
		doNotAttemptToolInstall[cmd] = true // either it's installed or it's not--either way, we're done with it
	}
	return nil
}

// Command safety check cache to avoid repeated LLM calls
var (
	autoInstallMu           sync.Mutex
	doNotAttemptToolInstall = make(map[string]bool) // set to true if the tool should not be auto-installed
)

// installTools installs missing tools.
func (b *BashTool) installTools(ctx context.Context, missing []string) error {
	slog.InfoContext(ctx, "installTools subconvo", "tools", missing)

	info := conversation.ToolCallInfoFromContext(ctx)
	if info.Convo == nil {
		return fmt.Errorf("no conversation context available for tool installation")
	}
	subConvo := info.Convo.SubConvo()
	subConvo.Hidden = true
	subBash := NewBashTool(nil, NoBashToolJITInstall)

	done := false
	doneTool := &llm.Tool{
		Name:        "done",
		Description: "Call this tool once when finished processing all commands, providing the installation status for each.",
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "results": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "command_name": {
            "type": "string",
            "description": "The name of the command"
          },
          "installed": {
            "type": "boolean",
            "description": "Whether the command was installed"
          }
        },
        "required": ["command_name", "installed"]
      }
    }
  },
  "required": ["results"]
}`),
		Run: func(ctx context.Context, input json.RawMessage) ([]llm.Content, error) {
			type InstallResult struct {
				CommandName string `json:"command_name"`
				Installed   bool   `json:"installed"`
			}
			type DoneInput struct {
				Results []InstallResult `json:"results"`
			}
			var doneInput DoneInput
			err := json.Unmarshal(input, &doneInput)
			results := doneInput.Results
			if err != nil {
				slog.WarnContext(ctx, "failed to parse install results", "raw", string(input), "error", err)
			} else {
				slog.InfoContext(ctx, "auto-tool installation complete", "results", results)
			}
			done = true
			return llm.TextContent(""), nil
		},
	}

	subConvo.Tools = []*llm.Tool{
		subBash,
		doneTool,
	}

	const autoinstallSystemPrompt = `The assistant powers an entirely automated auto-installer tool.

The user will provide a list of commands that were not found on the system.

The assistant's task:

First, decide whether each command is mainstream and safe for automatic installation in a development environment. Skip any commands that could cause security issues, legal problems, or consume excessive resources.

For each appropriate command:

1. Detect the system's package manager and install the command using standard repositories only (no source builds, no curl|bash installs).
2. Make a minimal verification attempt (package manager success is sufficient).
3. If installation fails after reasonable attempts, mark as failed and move on.

Once all commands have been processed, call the "done" tool with the status of each command.
`

	subConvo.SystemPrompt = autoinstallSystemPrompt

	cmds := new(strings.Builder)
	cmds.WriteString("<commands>\n")
	for _, cmd := range missing {
		cmds.WriteString("<command>")
		cmds.WriteString(cmd)
		cmds.WriteString("</command>\n")
	}
	cmds.WriteString("</commands>\n")

	resp, err := subConvo.SendUserTextMessage(cmds.String())
	if err != nil {
		return err
	}

	for !done {
		if resp.StopReason != llm.StopReasonToolUse {
			return fmt.Errorf("subagent finished without calling tool")
		}

		ctxWithWorkDir := WithWorkingDir(ctx, WorkingDir(ctx))
		results, _, err := subConvo.ToolResultContents(ctxWithWorkDir, resp)
		if err != nil {
			return err
		}

		resp, err = subConvo.SendMessage(llm.Message{
			Role:    llm.MessageRoleUser,
			Content: results,
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// cleanPtyOutput processes PTY command output to remove shell prompts, command echoes, and leading empty lines, returning a cleaner result string.
func cleanPtyOutput(output, command string) string {
	lines := strings.Split(output, "\n")
	var cleanLines []string

	skipNext := false
	for i, line := range lines {
		if skipNext {
			skipNext = false
			continue
		}

		// Skip common shell prompts (basic heuristic)
		if strings.HasPrefix(line, "$ ") || strings.HasPrefix(line, "# ") || strings.HasPrefix(line, "> ") {
			continue
		}

		// Skip the command echo if it appears at the beginning
		if i < 3 && strings.Contains(line, command) {
			continue
		}

		// Skip exit command echo
		if strings.Contains(line, "; exit $?") {
			continue
		}

		// Skip empty lines at the beginning
		if len(cleanLines) == 0 && strings.TrimSpace(line) == "" {
			continue
		}

		cleanLines = append(cleanLines, line)
	}

	// Join back and trim trailing whitespace
	result := strings.Join(cleanLines, "\n")
	return strings.TrimRight(result, "\n")
}
