package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/benl1006/Autonomous-CI-Platform/orchestrator/internal/config"
	"github.com/benl1006/Autonomous-CI-Platform/orchestrator/internal/dockertools"
	"github.com/benl1006/Autonomous-CI-Platform/orchestrator/internal/pipelines"
	"github.com/benl1006/Autonomous-CI-Platform/orchestrator/internal/servertools"
	"github.com/benl1006/Autonomous-CI-Platform/orchestrator/internal/types"

	dockerClient "github.com/moby/moby/client"
)

func main() {
	// Initialize environment variables to global scope
	if err := config.Init(); err != nil {
		slog.Error("Failed to initialize global variables", "error", err)
		return
	}

	// Create log directory if it does not yet exist
	logDirPath := config.RelToAbsPath("logs")
	if err := os.MkdirAll(logDirPath, 0755); err != nil {
		fmt.Printf("Failed to create log directory: %v\n", err)
		return
	}

	// slogs are currently printed to stdout and the log folder
	timestamp := time.Now().Format("2006-01-02_15-04-05")
	logFilename := fmt.Sprintf("orchestrator_%s.log", timestamp)
	logFile, err := os.OpenFile(filepath.Join(logDirPath, logFilename), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		fmt.Printf("Failed to open log file: %v\n", err)
		return
	}
	defer logFile.Close()
	multiLogger := io.MultiWriter(os.Stdout, logFile)
	logger := slog.New(slog.NewJSONHandler(multiLogger, nil))
	slog.SetDefault(logger)
	mainCtx := context.Background()
	prChan := make(chan *types.PullRequest)
	aierChan := make(chan *types.AIEngineResponse)
	pcMap := types.NewPushedCommits()
	wfm := pipelines.NewWorkflowManager()

	cli, err := dockerClient.New(dockerClient.FromEnv)
	if err != nil {
		slog.Error("Failed to start moby client", "error", err)
		return
	}
	defer func() {
		if err := cli.Close(); err != nil {
			slog.Error("Failed to close the docker client", "error", err)
		}
	}()

	if err := dockertools.WaitForDocker(mainCtx, cli, time.Duration(config.DockerStartTimeout)*time.Second); err != nil {
		slog.Error("Docker daemon unreachable", "error", err)
		return
	}

	if err := dockertools.ClearOldContainers(mainCtx, cli); err != nil {
		slog.Error("Failed to clean old containers", "error", err)
		return
	}

	if err := dockertools.ClearOldImages(mainCtx, cli); err != nil {
		slog.Error("Failed to clean old images", "error", err)
		return
	}

	go wfm.RunWorkflowPipeline(mainCtx, cli, prChan, aierChan, pcMap)

	if err := servertools.StartServer(mainCtx, prChan, aierChan, pcMap); err != nil {
		slog.Error("Server failure", "error", err)
		return
	}
}

/*
TODO List:
	- handle hanging workflows due to errors: currently we just drop the whole workflow with no retry
	- handle dead containers
	- list testing and platform limitations and conditions; close platfrom if conditions are not met
	- currently seeds rag pipeline on start every time; fine for now, but when persistance is added, must be addressed
	- implement seeding the rag pipeline 

Wishlist
	- persistance
		- add docker volumes
		- workspaces
		- pushed commits
		- workflows
		- log storage
	- multi-service testing
	- seperate test files/test directory structure
	- running tests multiple times?
	- structured test result contracts
	- orchestrator can check the language from a user provided spec?

*/
