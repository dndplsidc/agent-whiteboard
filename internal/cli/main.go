package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/edocsss/agent-whiteboard/internal/app"
	"github.com/edocsss/agent-whiteboard/internal/common"
	httpx "github.com/edocsss/agent-whiteboard/internal/http"
)

const (
	exitSuccess  = 0
	exitInternal = 1
	exitUsage    = 2
	exitRemote   = 3
	exitContext  = 4
)

func Run(ctx context.Context, stdout, stderr io.Writer, getenv func(string) string) int {
	deps := Dependencies{
		Stdout: stdout,
		Stderr: stderr,
		Getenv: getenv,
		NewClient: func(config httpx.ClientConfig) (Client, error) {
			return httpx.NewClient(config)
		},
		NewApplication: func(config app.ServiceConfig, options ...app.Option) (Application, error) {
			return app.NewService(config, options...)
		},
		NewAgentApplication: func(config app.AgentServiceConfig) (Application, error) {
			return app.NewAgentService(config)
		},
	}
	return run(ctx, stdout, stderr, getenv, os.Args[1:], deps)
}

func run(ctx context.Context, stdout, stderr io.Writer, getenv func(string) string, args []string, deps Dependencies) int {
	root, err := NewRoot(deps)
	if err != nil {
		writeCommandError(stdout, stderr, requestedJSON(args), err)
		return commandExitCode(err)
	}
	root.SetArgs(args)
	if _, _, findErr := root.Find(args); findErr != nil {
		err = markUsage(findErr)
	} else {
		err = root.ExecuteContext(ctx)
	}
	if err == nil {
		return exitSuccess
	}
	jsonMode, _ := root.PersistentFlags().GetBool("json")
	writeCommandError(stdout, stderr, jsonMode || requestedJSON(args), err)
	return commandExitCode(err)
}

func commandExitCode(err error) int {
	if err == nil {
		return exitSuccess
	}
	if _, contextOnly := contextOnlyError(err); contextOnly {
		return exitContext
	}
	var usage usageError
	if errors.As(err, &usage) {
		return exitUsage
	}
	var domain *common.Error
	if errors.As(err, &domain) {
		return exitRemote
	}
	return exitInternal
}

func requestedJSON(args []string) bool {
	requested := false
	for _, argument := range args {
		if argument == "--" {
			break
		}
		if argument == "--json" {
			requested = true
			continue
		}
		if value, found := strings.CutPrefix(argument, "--json="); found {
			parsed, err := strconv.ParseBool(value)
			if err == nil {
				requested = parsed
			}
		}
	}
	return requested
}

func Main() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := Run(ctx, os.Stdout, os.Stderr, os.Getenv)
	stop()
	return code
}
