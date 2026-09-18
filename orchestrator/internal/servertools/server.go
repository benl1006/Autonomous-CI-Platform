package servertools

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"time"

	"github.com/benl1006/Autonomous-CI-Platform/orchestrator/internal/config"
	"github.com/benl1006/Autonomous-CI-Platform/orchestrator/internal/types"
)

const githubAPIVersion = "2026-03-10"

// Generates the HMAC key based on the message and secret
func generateHMAC(message []byte, secret string) (string, error) {
	hash := hmac.New(sha256.New, []byte(secret))
	_, err := hash.Write(message)
	if err != nil {
		return "", fmt.Errorf("Failed to write HMAC hash: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// Verifies if the message is legitimate
func verifyMessage(message []byte, secret, actualSig string) (bool, error) {
	realSig, err := generateHMAC(message, secret)
	if err != nil {
		return false, fmt.Errorf("Failed to verify HMAC signature: %w", err)
	}
	return hmac.Equal([]byte(actualSig), []byte(realSig)), err
}

// Returns time.Duration of n seconds
func seconds(n int) time.Duration {
	return time.Duration(n) * time.Second
}

// Github webhook handler
func whHandler(prChan chan<- *types.PullRequest, pc *types.PushedCommits) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			closeErr := r.Body.Close()
			if closeErr != nil {
				slog.Error("Failed to close request body", "error", closeErr)
			}
		}()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			slog.Error("Failed to read request body", "error", err)
			return
		}

		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			slog.Error("Not a post request", "error", err)
			return
		}

		if r.Header.Get("Content-Type") != "application/json" {
			slog.Error("Webhook body must be JSON")
			return
		}

		actualSig := strings.TrimPrefix(r.Header.Get("X-Hub-Signature-256"), "sha256=")
		verified, err := verifyMessage(body, config.GithubSecret, actualSig)
		if err != nil {
			slog.Error("Failed to verify message", "error", err)
			return
		}
		if !verified {
			w.WriteHeader(http.StatusUnauthorized)
			slog.Warn("Unauthorized request", "warn", err)
			return
		}

		if r.Header.Get("X-GitHub-Event") != "pull_request" {
			slog.Error("Not a pull request")
			return
		}

		var pr types.PullRequest
		if err := pr.UnmarshalPullRequest(body); err != nil {
			slog.Error("Failed to unmsarhsal the pull request", "error", err)
			return
		}

		if pc.IsSelfPush(pr.Number, pr.HeadSHA) {
			slog.Info("Webhook originates from this platform")
			return
		}

		slog.Info("Webhook recieved", "pull request", pr)
		prChan <- &pr
	}
}

// Handles responses from the AI Engine and sends response to respChannel.
func aiEngineResponseHandler(aierChan chan<- *types.AIEngineResponse) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if closeErr := r.Body.Close(); closeErr != nil {
				slog.Error("Failed to close request body", "error", closeErr)
			}
		}()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			slog.Error("Failed to read request body", "error", err)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			slog.Error("Not a post request", "error", err)
			return
		}

		if r.Header.Get("Content-Type") != "application/json" {
			slog.Error("Webhook body must be JSON")
			return
		}

		actualSig := r.Header.Get("HMAC-Signature-256")
		verified, err := verifyMessage(body, config.InternalSecret, actualSig)
		if err != nil {
			slog.Error("Failed to verify message", "error", err)
			return
		}
		if !verified {
			w.WriteHeader(http.StatusUnauthorized)
			slog.Warn("Unauthorized request", "warn", err)
			return
		}

		var resp types.AIEngineResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			w.WriteHeader(http.StatusUnsupportedMediaType)
			slog.Error("Could not unmarshal the data into a type.Response", "error", err)
			return
		}
		slog.Info("AI Engine response recived", "aier", resp)
		aierChan <- &resp
	}
}

// Sends a http request to the AI Engine.
// jobType can be one of: "open", "close", "test_results", "edit", "sync".
// open: Start a workflow when a pr opens.
// close: Close and merge implied; end associated workflow and update rag index.
// logs: Return the logs of the last test run.
// edit: Update pr information.
// sync: Update branch head
func SendRequestAIEngine(ctx context.Context, aiEngineJobType string, req types.AIEngineRequest) (err error) {
	if !slices.Contains(config.AiEngineRequestJobTypes, aiEngineJobType) {
		return fmt.Errorf("Invalid job type")
	}

	cli := http.Client{
		Timeout: seconds(config.RequestTimeout),
	}

	msgBytes, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("Failed to marshal the message package: %w", err)
	}
	newCtx, cancel := context.WithTimeout(ctx, seconds(config.RequestTimeout))
	defer cancel()
	msgReader := bytes.NewReader(msgBytes)
	httpReq, err := http.NewRequestWithContext(newCtx, http.MethodPost, config.AIEngineURL, msgReader)
	if err != nil {
		return fmt.Errorf("Failed to create http request: %w", err)
	}

	hmacSig, err := generateHMAC(msgBytes, config.InternalSecret)
	if err != nil {
		return fmt.Errorf("Failed to generate HMAC: %w", err)
	}
	httpReq.Header.Set("HMAC-Signature-256", hmacSig)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Job-Type", aiEngineJobType)

	resp, err := cli.Do(httpReq)
	if err != nil {
		return fmt.Errorf("Failed to send http request: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body) // drain so the connection can be reused
		if closeErr := resp.Body.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Bad response, status: %v", resp.StatusCode)
	}
	slog.Info("Request sent to AI engine", "jobtype", aiEngineJobType, "aier", req)
	return nil
}

// Sends the workspace content to the AI Engine to seed the RAG pipeline.
func SeedRagPipeline(ctx context.Context, files []types.ChangedFile) (err error) {
	filesBytes, err := json.Marshal(files)
	if err != nil {
		return fmt.Errorf("Failed to marshal seed: %w", err)
	}

	hmacSig, err := generateHMAC(filesBytes, config.InternalSecret)
	if err != nil {
		return fmt.Errorf("Failed to generate HMAC: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, config.AIEngineURL, bytes.NewReader(filesBytes))
	if err != nil {
		return fmt.Errorf("Failed to create http request: %w", err)
	}
	httpReq.Header.Set("HMAC-Signature-256", hmacSig)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Job-Type", config.AiEngineSeedJobType)

	cli := http.Client{
		Timeout: seconds(config.RequestTimeout),
	}
	resp, err := cli.Do(httpReq)
	if err != nil {
		return fmt.Errorf("Failed to send http request: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body) // drain so the connection can be reused
		if closeErr := resp.Body.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Bad response, status: %v", resp.StatusCode)
	}
	slog.Info("Seed sent to AI engine", "jobtype", config.AiEngineSeedJobType)
	return nil
}

// Posts a comment on the pull request for the results of the test.
func PostSummaryComment(ctx context.Context, commentsURL string, body string) (err error) {
	cli := http.Client{
		Timeout: seconds(config.RequestTimeout),
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, commentsURL, strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("Failed to create http request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+config.GithubToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	req.Header.Set("Content-Type", "application/json")

	resp, err := cli.Do(req)
	if err != nil {
		return fmt.Errorf("Failed to send http request: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body) // drain so the connection can be reused
		if closeErr := resp.Body.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()

	if resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("Unexpected status code %v posting comment: %s", resp.StatusCode, respBody)
	}

	slog.Info("Summary comment posted", "comment", body)
	return nil
}

// Starts the http server.
func StartServer(ctx context.Context, prChan chan<- *types.PullRequest, aierChan chan<- *types.AIEngineResponse, pc *types.PushedCommits) (err error) {

	// initialize server
	mux := http.NewServeMux()
	mux.Handle("/", http.HandlerFunc(whHandler(prChan, pc)))
	mux.Handle("/aiengine", http.HandlerFunc(aiEngineResponseHandler(aierChan)))

	port := fmt.Sprintf(":%s", config.Port)
	server := &http.Server{
		Addr:              port,
		Handler:           mux,                               // Inject your isolated router
		ReadHeaderTimeout: seconds(config.ReadHeaderTimeout), // Max time to read just the headers
		WriteTimeout:      seconds(config.WriteTimeout),      // Max time to write the response
	}

	// create a context for server lifetime
	lifetime, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	// start the server
	go func() {
		slog.Info("Server is starting", "port", server.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("Server error", "error", err)
			os.Exit(1)
		}
	}()

	// block until an os.Interrupt occurs
	<-lifetime.Done()

	// create a context for duration of the server shutdown
	shutdownCtx, cancelShutdown := context.WithTimeout(ctx, seconds(config.ServerShutdownTimeout))
	defer cancelShutdown()

	// shutting down the server
	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("Server forced to shutdown", "error", err)
		return err
	}

	slog.Info("Server shutdown complete!")
	return nil
}
