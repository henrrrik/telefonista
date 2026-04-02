package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type mockUploader struct {
	called bool
	input  *s3.PutObjectInput
	err    error
}

func (m *mockUploader) PutObject(_ context.Context, input *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	m.called = true
	m.input = input
	return &s3.PutObjectOutput{}, m.err
}

type mockTranscriber struct {
	called bool
	text   string
	err    error
}

func (m *mockTranscriber) Transcribe(_ context.Context, _ []byte, _ string) (string, error) {
	m.called = true
	return m.text, m.err
}

func testConfig() Configuration {
	return Configuration{
		SlackName:       "testbot",
		SlackIconURL:    "https://example.com/icon.png",
		SlackChannel:    "#test",
		Host:            "https://example.com",
		VoicemailAudio:  "https://example.com/greeting.wav",
		ElksUserName:    "testuser",
		ElksPassword:    "testpass",
		S3Endpoint:      "https://s3.example.com",
		S3BucketName:    "test-bucket",
	}
}

func newElksServer(data []byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "testuser" || pass != "testpass" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Write(data)
	}))
}

func TestHealth(t *testing.T) {
	mux := newMux(testConfig(), http.DefaultClient, &mockUploader{}, nil)

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("expected status=ok, got %q", body["status"])
	}
}

func TestWebhookSecretRejectsInvalidSecret(t *testing.T) {
	cfg := testConfig()
	cfg.WebhookSecret = "mysecret"
	mux := newMux(cfg, http.DefaultClient, &mockUploader{}, nil)

	req := httptest.NewRequest("POST", "/incoming_call", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without secret, got %d", w.Code)
	}

	req = httptest.NewRequest("POST", "/incoming_call?secret=wrong", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong secret, got %d", w.Code)
	}
}

func TestWebhookSecretAcceptsValidSecret(t *testing.T) {
	cfg := testConfig()
	cfg.WebhookSecret = "mysecret"
	mux := newMux(cfg, http.DefaultClient, &mockUploader{}, nil)

	req := httptest.NewRequest("POST", "/incoming_call?secret=mysecret", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with valid secret, got %d", w.Code)
	}
}

func TestWebhookSecretNotRequiredWhenUnset(t *testing.T) {
	cfg := testConfig()
	cfg.WebhookSecret = ""
	mux := newMux(cfg, http.DefaultClient, &mockUploader{}, nil)

	req := httptest.NewRequest("POST", "/incoming_call", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 without secret configured, got %d", w.Code)
	}
}

func TestIncomingCallIncludesSecretInCallbackURL(t *testing.T) {
	cfg := testConfig()
	cfg.WebhookSecret = "mysecret"
	mux := newMux(cfg, http.DefaultClient, &mockUploader{}, nil)

	req := httptest.NewRequest("POST", "/incoming_call?secret=mysecret", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	var resp IncomingResponse
	json.NewDecoder(w.Body).Decode(&resp)

	expected := cfg.Host + "/voicemail?secret=mysecret"
	if resp.Next.Record != expected {
		t.Fatalf("expected callback URL %q, got %q", expected, resp.Next.Record)
	}
}

func TestIncomingCall(t *testing.T) {
	cfg := testConfig()
	mux := newMux(cfg, http.DefaultClient, &mockUploader{}, nil)

	req := httptest.NewRequest("POST", "/incoming_call", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("expected JSON content type, got %q", ct)
	}

	var resp IncomingResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Play != cfg.VoicemailAudio {
		t.Errorf("expected play=%q, got %q", cfg.VoicemailAudio, resp.Play)
	}
	if resp.Next.Record != cfg.Host+"/voicemail" {
		t.Errorf("expected next.record=%q, got %q", cfg.Host+"/voicemail", resp.Next.Record)
	}
}

func TestIncomingCallRejectsGET(t *testing.T) {
	mux := newMux(testConfig(), http.DefaultClient, &mockUploader{}, nil)

	req := httptest.NewRequest("GET", "/incoming_call", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Fatal("expected non-200 for GET request")
	}
}

func TestVoicemailMissingFields(t *testing.T) {
	mux := newMux(testConfig(), http.DefaultClient, &mockUploader{}, nil)

	tests := []struct {
		name   string
		values url.Values
	}{
		{"missing both", url.Values{}},
		{"missing wav", url.Values{"from": {"+46123456"}}},
		{"missing from", url.Values{"wav": {"https://example.com/audio.wav"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/voicemail", strings.NewReader(tt.values.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400, got %d", w.Code)
			}
		})
	}
}

func TestVoicemailSuccess(t *testing.T) {
	wavData := []byte("fake-wav-data")
	elksServer := newElksServer(wavData)
	defer elksServer.Close()

	var slackBody []byte
	slackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slackBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer slackServer.Close()

	cfg := testConfig()
	cfg.SlackWebHookURL = slackServer.URL

	uploader := &mockUploader{}
	tr := &mockTranscriber{text: "Hello, this is a test message."}
	mux := newMux(cfg, elksServer.Client(), uploader, tr)

	form := url.Values{
		"from": {"+46701234567"},
		"wav":  {elksServer.URL + "/recording.wav"},
	}
	req := httptest.NewRequest("POST", "/voicemail", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Verify S3 upload
	if !uploader.called {
		t.Fatal("expected S3 upload to be called")
	}
	if *uploader.input.Bucket != "test-bucket" {
		t.Errorf("expected bucket=test-bucket, got %q", *uploader.input.Bucket)
	}
	if *uploader.input.ContentType != "audio/wav" {
		t.Errorf("expected content-type=audio/wav, got %q", *uploader.input.ContentType)
	}
	uploadedBody, _ := io.ReadAll(uploader.input.Body)
	if string(uploadedBody) != string(wavData) {
		t.Errorf("uploaded data mismatch: got %q", uploadedBody)
	}

	// Verify transcription was called
	if !tr.called {
		t.Fatal("expected transcriber to be called")
	}

	// Verify Slack message contains caller, URL, and transcription
	var slackPayload SlackPayload
	if err := json.Unmarshal(slackBody, &slackPayload); err != nil {
		t.Fatalf("failed to decode Slack payload: %v", err)
	}
	if slackPayload.Channel != "#test" {
		t.Errorf("expected Slack channel=#test, got %q", slackPayload.Channel)
	}
	if !strings.Contains(slackPayload.Text, "+46701234567") {
		t.Errorf("expected Slack message to contain caller number, got %q", slackPayload.Text)
	}
	if !strings.Contains(slackPayload.Text, "s3.example.com/test-bucket/voicemail/") {
		t.Errorf("expected Slack message to contain S3 URL, got %q", slackPayload.Text)
	}
	if !strings.Contains(slackPayload.Text, "Hello, this is a test message.") {
		t.Errorf("expected Slack message to contain transcription, got %q", slackPayload.Text)
	}
}

func TestVoicemailNoTranscriber(t *testing.T) {
	wavData := []byte("fake-wav-data")
	elksServer := newElksServer(wavData)
	defer elksServer.Close()

	var slackBody []byte
	slackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slackBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer slackServer.Close()

	cfg := testConfig()
	cfg.SlackWebHookURL = slackServer.URL

	uploader := &mockUploader{}
	mux := newMux(cfg, elksServer.Client(), uploader, nil)

	form := url.Values{
		"from": {"+46701234567"},
		"wav":  {elksServer.URL + "/recording.wav"},
	}
	req := httptest.NewRequest("POST", "/voicemail", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Slack message should not contain a quote block
	var slackPayload SlackPayload
	if err := json.Unmarshal(slackBody, &slackPayload); err != nil {
		t.Fatalf("failed to decode Slack payload: %v", err)
	}
	if strings.Contains(slackPayload.Text, "\n>") {
		t.Errorf("expected no transcription quote, got %q", slackPayload.Text)
	}
}

func TestVoicemailTranscriptionFailureStillSucceeds(t *testing.T) {
	wavData := []byte("fake-wav-data")
	elksServer := newElksServer(wavData)
	defer elksServer.Close()

	var slackBody []byte
	slackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slackBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer slackServer.Close()

	cfg := testConfig()
	cfg.SlackWebHookURL = slackServer.URL

	uploader := &mockUploader{}
	tr := &mockTranscriber{err: errors.New("whisper API error")}
	mux := newMux(cfg, elksServer.Client(), uploader, tr)

	form := url.Values{
		"from": {"+46701234567"},
		"wav":  {elksServer.URL + "/recording.wav"},
	}
	req := httptest.NewRequest("POST", "/voicemail", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	// Should still succeed — transcription failure is non-fatal
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !uploader.called {
		t.Fatal("expected S3 upload to be called")
	}

	// Slack message should be sent without transcription
	var slackPayload SlackPayload
	if err := json.Unmarshal(slackBody, &slackPayload); err != nil {
		t.Fatalf("failed to decode Slack payload: %v", err)
	}
	if strings.Contains(slackPayload.Text, "\n>") {
		t.Errorf("expected no transcription quote on failure, got %q", slackPayload.Text)
	}
}

func TestVoicemailTooLarge(t *testing.T) {
	// Create a server that returns more than 50MB
	bigData := make([]byte, 50*1024*1024+1)
	elksServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(bigData)
	}))
	defer elksServer.Close()

	cfg := testConfig()
	uploader := &mockUploader{}
	mux := newMux(cfg, elksServer.Client(), uploader, nil)

	form := url.Values{
		"from": {"+46701234567"},
		"wav":  {elksServer.URL + "/recording.wav"},
	}
	req := httptest.NewRequest("POST", "/voicemail", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for oversized WAV, got %d", w.Code)
	}
	if uploader.called {
		t.Fatal("S3 upload should not be called for oversized files")
	}
}

func TestVoicemailS3Failure(t *testing.T) {
	elksServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("fake-wav"))
	}))
	defer elksServer.Close()

	cfg := testConfig()
	uploader := &mockUploader{err: io.ErrUnexpectedEOF}
	mux := newMux(cfg, elksServer.Client(), uploader, nil)

	form := url.Values{
		"from": {"+46701234567"},
		"wav":  {elksServer.URL + "/recording.wav"},
	}
	req := httptest.NewRequest("POST", "/voicemail", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on S3 failure, got %d", w.Code)
	}
}

func TestVoicemailSlackFailureStillSucceeds(t *testing.T) {
	elksServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("fake-wav"))
	}))
	defer elksServer.Close()

	cfg := testConfig()
	cfg.SlackWebHookURL = "http://127.0.0.1:1"

	uploader := &mockUploader{}
	mux := newMux(cfg, elksServer.Client(), uploader, nil)

	form := url.Values{
		"from": {"+46701234567"},
		"wav":  {elksServer.URL + "/recording.wav"},
	}
	req := httptest.NewRequest("POST", "/voicemail", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 even when Slack fails, got %d", w.Code)
	}
	if !uploader.called {
		t.Fatal("expected S3 upload to still be called")
	}
}
