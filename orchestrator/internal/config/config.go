package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// Add new global variables by adding it below, and set the env override in loadEnv.
var (
	WsDir string

	OrchRootDir             string
	Port                    string = "8080"
	GithubToken             string
	RepositoryUrl           string
	GithubSecret            string
	InternalSecret          string
	AIEngineURL             string = "http://localhost:8000"
	RequestTimeout          int    = 5   // seconds
	ServerShutdownTimeout   int    = 30  // seconds
	ReadHeaderTimeout       int    = 2   // seconds
	WriteTimeout            int    = 5   // seconds
	ContainerTimeout        int    = 10  // minutes
	RequestCloseTimeout     int    = 10  // seconds
	DockerStartTimeout      int    = 10  // seconds
	ContainerMemoryCap      int    = 512 // MB
	ListChangedFilesTimeout int    = 10  // seconds
	MaxTestPatchingAttempts int    = 10
	TestingEnvSlice         []string

	AiEngineResponseJobTypes = []string{"run_tests", "commit_push"}
	WebhookJobTypes          = []string{"open", "edit", "sync"}
	AiEngineRequestJobTypes  = []string{"open", "close", "test_results", "edit", "sync"}
	AiEngineSeedJobType      = "seed"
)

const (
	BYTE int = 1
	KB   int = 1e3 * BYTE
	MB   int = 1e3 * KB
	GB   int = 1e3 * MB
)

// Sets the root directory.
func resolveRootDir() error {
	// Use env variable for the project root if it exists. Useful for
	// containers/CI where the marker-based walk isn't desired or possible.
	if root := os.Getenv("ORCHESTRATOR_ROOT"); root != "" {
		OrchRootDir = root
		return nil
	}

	dir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("Failed to get working directory: %w", err)
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, "config", "test-env-vars.txt")); err == nil {
			OrchRootDir = dir
			return nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return fmt.Errorf("Could not locate orchestrator root (no config/test-env-vars.txt found)")
		}
		dir = parent
	}
}

// Loads the environment variables to be injected into the docker container for testing.
func loadTestingEnvVars() error {
	// Target the nested config directory
	envPath := RelToAbsPath("config", "test-env-vars.txt")

	testEnvFile, err := os.Open(envPath)
	if err != nil {
		return fmt.Errorf("Failed to open test env file at %q: %w", envPath, err)
	}
	defer testEnvFile.Close()

	testEnvScanner := bufio.NewScanner(testEnvFile)
	for testEnvScanner.Scan() {
		line := strings.TrimSpace(testEnvScanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		TestingEnvSlice = append(TestingEnvSlice, line)
	}

	if err := testEnvScanner.Err(); err != nil {
		return fmt.Errorf("scanner Failed: %w", err)
	}
	return nil
}

// Loads the environment variables.
func loadEnv() error {
	// Root level .env file
	envPath := RelToAbsPath(".env")
	if err := godotenv.Load(envPath); err != nil {
		// Non-fatal if running in environments where variables are injected directly (e.g., Docker/K8s)
		if !os.IsNotExist(err) {
			return fmt.Errorf("Failed to load .env file from %q: %w", envPath, err)
		}
	}

	// Environment variable assignments with fallback defaults
	GithubToken = os.Getenv("GITHUB_TOKEN")
	RepositoryUrl = os.Getenv("GITHUB_REPOSITORY_URL")
	GithubSecret = os.Getenv("GITHUB_WEBHOOK_SECRET")
	InternalSecret = os.Getenv("INTERNAL_SECRET")

	if p := os.Getenv("PORT"); p != "" {
		Port = p
	}

	if aiURL := os.Getenv("AI_ENGINE_URL"); aiURL != "" {
		AIEngineURL = aiURL
	}

	// Optional numeric overrides from environment

	if valAiTimeout := os.Getenv("REQUEST_TIMEOUT"); valAiTimeout != "" {
		parsedVal, err := strconv.Atoi(valAiTimeout)
		if err == nil {
			RequestTimeout = parsedVal
		}
	}

	if valListChangedFilesTimeout := os.Getenv("LIST_CHANGED_FILES_TIMEOUT"); valListChangedFilesTimeout != "" {
		parsedVal, err := strconv.Atoi(valListChangedFilesTimeout)
		if err == nil {
			ListChangedFilesTimeout = parsedVal
		}
	}

	if valServerShutdown := os.Getenv("SERVER_SHUTDOWN_TIMEOUT"); valServerShutdown != "" {
		parsedVal, err := strconv.Atoi(valServerShutdown)
		if err == nil {
			ServerShutdownTimeout = parsedVal
		}
	}

	if valReadHeader := os.Getenv("READ_HEADER_TIMEOUT"); valReadHeader != "" {
		parsedVal, err := strconv.Atoi(valReadHeader)
		if err == nil {
			ReadHeaderTimeout = parsedVal
		}
	}

	if valWriteTimeout := os.Getenv("WRITE_TIMEOUT"); valWriteTimeout != "" {
		parsedVal, err := strconv.Atoi(valWriteTimeout)
		if err == nil {
			WriteTimeout = parsedVal
		}
	}

	if valAiTimeout := os.Getenv("AI_ENGINE_REQUEST_CLOSE_TIMEOUT"); valAiTimeout != "" {
		parsedVal, err := strconv.Atoi(valAiTimeout)
		if err == nil {
			RequestCloseTimeout = parsedVal
		}
	}

	if valContainerTimeout := os.Getenv("CONTAINER_TIMEOUT"); valContainerTimeout != "" {
		parsedVal, err := strconv.Atoi(valContainerTimeout)
		if err == nil {
			ContainerTimeout = parsedVal
		}
	}

	if valDockerTimeout := os.Getenv("DOCKER_START_TIMEOUT"); valDockerTimeout != "" {
		parsedVal, err := strconv.Atoi(valDockerTimeout)
		if err == nil {
			DockerStartTimeout = parsedVal
		}
	}

	if valContainerMemoryCap := os.Getenv("CONTAINER_MEMORY_CAP"); valContainerMemoryCap != "" {
		parsedVal, err := strconv.Atoi(valContainerMemoryCap)
		if err == nil {
			ContainerMemoryCap = parsedVal
		}
	}

	if valMaxTestAttempts := os.Getenv("MAX_TEST_PATCHING_ATTEMPTS"); valMaxTestAttempts != "" {
		parsedVal, err := strconv.Atoi(valMaxTestAttempts)
		if err == nil {
			MaxTestPatchingAttempts = parsedVal
		}
	}

	return nil
}

// Returns an error if critical environment variables are missing.
func validateConfig() error {
	var missing []string

	if GithubToken == "" {
		missing = append(missing, "GITHUB_TOKEN")
	}
	if RepositoryUrl == "" {
		missing = append(missing, "GITHUB_REPOSITORY_URL")
	}
	if GithubSecret == "" {
		missing = append(missing, "GITHUB_WEBHOOK_SECRET")
	}
	if InternalSecret == "" {
		missing = append(missing, "INTERNAL_SECRET")
	}

	if len(missing) > 0 {
		return fmt.Errorf("Missing required environment variables: %q", strings.Join(missing, ", "))
	}

	return nil
}

// Initializes the global variables.
func Init() error {

	if err := resolveRootDir(); err != nil {
		return fmt.Errorf("Failed to resolve root directory: %w", err)
	}

	if err := loadEnv(); err != nil {
		return fmt.Errorf("Failed to load env variables: %w", err)
	}

	if err := validateConfig(); err != nil {
		return fmt.Errorf("Invalid configuration: %w", err)
	}

	WsDir = RelToAbsPath("workspaces")

	if err := loadTestingEnvVars(); err != nil {
		return fmt.Errorf("Failed to load test env variables: %w", err)
	}

	return nil
}

// Joins and prefixes the orchestrator root to create the absolute path.
func RelToAbsPath(relPath ...string) string {
	return filepath.Join(append([]string{OrchRootDir}, relPath...)...)
}
