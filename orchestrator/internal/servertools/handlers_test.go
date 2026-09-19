package servertools

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/benl1006/Autonomous-CI-Platform/orchestrator/internal/config"
	"github.com/benl1006/Autonomous-CI-Platform/orchestrator/internal/types"
)

func githubPayload() []byte {
	return []byte(`{
		"action": "opened",
		"number": 11,
		"pull_request": {
			"title": "t",
			"body": "b",
			"head": {"ref": "feat", "sha": "aaa111"},
			"base": {"sha": "bbb222"},
			"merged": false
		}
	}`)
}

func TestWhHandler_AcceptsValidWebhook(t *testing.T) {
	prev := config.GithubSecret
	t.Cleanup(func() { config.GithubSecret = prev })
	config.GithubSecret = "whsec"

	body := githubPayload()
	sig, err := generateHMAC(body, config.GithubSecret)
	if err != nil {
		t.Fatal(err)
	}

	prChan := make(chan *types.PullRequest, 1)
	pc := types.NewPushedCommits()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", "sha256="+sig)
	req.Header.Set("X-GitHub-Event", "pull_request")
	rr := httptest.NewRecorder()

	whHandler(prChan, pc)(rr, req)

	select {
	case pr := <-prChan:
		if pr.Number != 11 || pr.Action != "opened" || pr.HeadSHA != "aaa111" {
			t.Errorf("unexpected PR: %+v", pr)
		}
	default:
		t.Fatal("expected pull request on channel")
	}
}

func TestWhHandler_Unauthorized(t *testing.T) {
	prev := config.GithubSecret
	t.Cleanup(func() { config.GithubSecret = prev })
	config.GithubSecret = "whsec"

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(githubPayload()))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", "sha256=00")
	req.Header.Set("X-GitHub-Event", "pull_request")
	rr := httptest.NewRecorder()

	whHandler(make(chan *types.PullRequest, 1), types.NewPushedCommits())(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

func TestWhHandler_MethodNotAllowed(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	whHandler(make(chan *types.PullRequest, 1), types.NewPushedCommits())(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}
}

func TestWhHandler_IgnoresSelfPush(t *testing.T) {
	prev := config.GithubSecret
	t.Cleanup(func() { config.GithubSecret = prev })
	config.GithubSecret = "whsec"

	body := githubPayload()
	sig, err := generateHMAC(body, config.GithubSecret)
	if err != nil {
		t.Fatal(err)
	}

	pc := types.NewPushedCommits()
	pc.Add(11, "aaa111")
	prChan := make(chan *types.PullRequest, 1)

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", "sha256="+sig)
	req.Header.Set("X-GitHub-Event", "pull_request")
	rr := httptest.NewRecorder()
	whHandler(prChan, pc)(rr, req)

	select {
	case <-prChan:
		t.Fatal("self-push should not be forwarded")
	default:
	}
}

func TestAIEngineResponseHandler_AcceptsValidPayload(t *testing.T) {
	prev := config.InternalSecret
	t.Cleanup(func() { config.InternalSecret = prev })
	config.InternalSecret = "aisec"

	payload := types.AIEngineResponse{Wfid: 3, Done: true, Summary: "ok"}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := generateHMAC(body, config.InternalSecret)
	if err != nil {
		t.Fatal(err)
	}

	ch := make(chan *types.AIEngineResponse, 1)
	req := httptest.NewRequest(http.MethodPost, "/aiengine", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("HMAC-Signature-256", sig)
	rr := httptest.NewRecorder()
	aiEngineResponseHandler(ch)(rr, req)

	select {
	case got := <-ch:
		if got.Wfid != 3 || !got.Done || got.Summary != "ok" {
			t.Errorf("got %+v", got)
		}
	default:
		t.Fatal("expected AI engine response on channel")
	}
}

func TestAIEngineResponseHandler_Unauthorized(t *testing.T) {
	prev := config.InternalSecret
	t.Cleanup(func() { config.InternalSecret = prev })
	config.InternalSecret = "aisec"

	req := httptest.NewRequest(http.MethodPost, "/aiengine", bytes.NewReader([]byte(`{"Wfid":1}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("HMAC-Signature-256", "nope")
	rr := httptest.NewRecorder()
	aiEngineResponseHandler(make(chan *types.AIEngineResponse, 1))(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}

func TestSendRequestAIEngine_InvalidJobType(t *testing.T) {
	err := SendRequestAIEngine(context.Background(), "not-a-job", types.AIEngineRequest{Wfid: 1})
	if err == nil {
		t.Fatal("expected invalid job type error")
	}
}

func TestPostSummaryComment_Success(t *testing.T) {
	prevToken, prevTimeout := config.GithubToken, config.RequestTimeout
	t.Cleanup(func() {
		config.GithubToken = prevToken
		config.RequestTimeout = prevTimeout
	})
	config.GithubToken = "gh-token"
	config.RequestTimeout = 2

	var (
		gotMethod      string
		gotAuth        string
		gotAccept      string
		gotContentType string
		gotBody        string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		gotContentType = r.Header.Get("Content-Type")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("ReadAll: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		gotBody = string(body)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)

	body := `{"body":"summary"}`
	if err := PostSummaryComment(context.Background(), srv.URL, body); err != nil {
		t.Fatalf("PostSummaryComment: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q, want %q", gotMethod, http.MethodPost)
	}
	if gotAuth != "Bearer gh-token" {
		t.Fatalf("Authorization = %q, want %q", gotAuth, "Bearer gh-token")
	}
	if gotAccept != "application/vnd.github+json" {
		t.Fatalf("Accept = %q, want %q", gotAccept, "application/vnd.github+json")
	}
	if gotContentType != "application/json" {
		t.Fatalf("Content-Type = %q, want %q", gotContentType, "application/json")
	}
	if gotBody != body {
		t.Fatalf("body = %q, want %q", gotBody, body)
	}
}

func TestPostSummaryComment_BadStatus(t *testing.T) {
	prevToken, prevTimeout := config.GithubToken, config.RequestTimeout
	t.Cleanup(func() {
		config.GithubToken = prevToken
		config.RequestTimeout = prevTimeout
	})
	config.GithubToken = "gh-token"
	config.RequestTimeout = 2

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("server exploded"))
	}))
	t.Cleanup(srv.Close)

	err := PostSummaryComment(context.Background(), srv.URL, `{"body":"summary"}`)
	if err == nil {
		t.Fatal("expected PostSummaryComment error for non-201 status")
	}
	if !strings.Contains(err.Error(), "Unexpected status code 500") {
		t.Fatalf("error = %q, want status text", err.Error())
	}
}

func TestSendRequestAIEngine_SuccessAndBadStatus(t *testing.T) {
	prevURL, prevSecret, prevTimeout := config.AIEngineURL, config.InternalSecret, config.RequestTimeout
	t.Cleanup(func() {
		config.AIEngineURL = prevURL
		config.InternalSecret = prevSecret
		config.RequestTimeout = prevTimeout
	})
	config.InternalSecret = "aisec"
	config.RequestTimeout = 2

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Job-Type") != "open" {
			t.Errorf("Job-Type = %q", r.Header.Get("Job-Type"))
		}
		if r.Header.Get("HMAC-Signature-256") == "" {
			t.Error("missing HMAC header")
		}
		var req types.AIEngineRequest
		jsonDecoder := json.NewDecoder(r.Body)
		if err := jsonDecoder.Decode(&req); err != nil {
			t.Errorf("body: %v", err)
		}
		if req.Wfid != 9 {
			t.Errorf("Wfid = %d", req.Wfid)
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	config.AIEngineURL = srv.URL

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := SendRequestAIEngine(ctx, "open", types.AIEngineRequest{Wfid: 9}); err != nil {
		t.Fatalf("SendRequestAIEngine: %v", err)
	}

	muxBad := http.NewServeMux()
	muxBad.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	bad := httptest.NewServer(muxBad)
	t.Cleanup(bad.Close)
	config.AIEngineURL = bad.URL
	if err := SendRequestAIEngine(ctx, "logs", types.AIEngineRequest{Wfid: 9}); err == nil {
		t.Fatal("expected error for non-200 response")
	}
}
