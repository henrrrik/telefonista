package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const (
	testWebhookUser = "webhookuser"
	testWebhookPass = "webhookpass"
)

type mockStore struct {
	called  bool
	input   *s3.PutObjectInput
	err     error
	objects map[string][]byte
	gets    int
}

func (m *mockStore) PutObject(_ context.Context, input *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	m.called = true
	m.input = input
	return &s3.PutObjectOutput{}, m.err
}

func (m *mockStore) GetObject(_ context.Context, input *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	m.gets++
	data, ok := m.objects[*input.Key]
	if !ok {
		return nil, &types.NoSuchKey{}
	}
	return &s3.GetObjectOutput{
		Body:          io.NopCloser(bytes.NewReader(data)),
		ContentLength: aws.Int64(int64(len(data))),
	}, nil
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

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parsing %q: %v", raw, err)
	}
	return u
}

func testConfig(t *testing.T) Configuration {
	return Configuration{
		SlackName:      "testbot",
		SlackIconURL:   "https://example.com/icon.png",
		SlackChannel:   "#test",
		Host:           mustParseURL(t, "https://example.com"),
		VoicemailAudio: "https://example.com/greeting.wav",
		ElksUserName:   "testuser",
		ElksPassword:   "testpass",
		S3Endpoint:     "https://s3.example.com",
		S3BucketName:   "test-bucket",
		WebhookUser:    testWebhookUser,
		WebhookPass:    testWebhookPass,
	}
}

func newElksServer(t *testing.T, data []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "testuser" || pass != "testpass" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// slackRecorder is a fake Slack webhook that records the payloads it receives.
type slackRecorder struct {
	*httptest.Server
	mu       sync.Mutex
	payloads []SlackPayload
}

func newSlackServer(t *testing.T, status int) *slackRecorder {
	t.Helper()
	rec := &slackRecorder{}
	rec.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p SlackPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Errorf("decoding Slack payload: %v", err)
		}
		rec.mu.Lock()
		rec.payloads = append(rec.payloads, p)
		rec.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(rec.Close)
	return rec
}

func (s *slackRecorder) only(t *testing.T) SlackPayload {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.payloads) != 1 {
		t.Fatalf("expected 1 Slack message, got %d", len(s.payloads))
	}
	return s.payloads[0]
}

// voicemailFixture wires a server to a fake 46elks and a fake Slack.
type voicemailFixture struct {
	srv   *server
	store *mockStore
	elks  *httptest.Server
	slack *slackRecorder
}

func newVoicemailFixture(t *testing.T, elks *httptest.Server, tr transcriber) *voicemailFixture {
	t.Helper()
	slack := newSlackServer(t, http.StatusOK)
	cfg := testConfig(t)
	cfg.SlackWebHookURL = slack.URL
	store := &mockStore{}
	srv := newServer(cfg, elks.Client(), store, tr)
	srv.wavOrigin = mustParseURL(t, elks.URL)
	return &voicemailFixture{srv: srv, store: store, elks: elks, slack: slack}
}

// post sends a voicemail callback and waits for background work to finish.
func (f *voicemailFixture) post(t *testing.T, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	w := postVoicemail(t, f.srv, form)
	if err := f.srv.waitForBackground(t.Context()); err != nil {
		t.Fatalf("waiting for background work: %v", err)
	}
	return w
}

func (f *voicemailFixture) defaultForm() url.Values {
	return url.Values{
		"from": {"+46701234567"},
		"wav":  {f.elks.URL + "/recording.wav"},
	}
}

func postVoicemail(t *testing.T, h http.Handler, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), "POST", "/voicemail", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(testWebhookUser, testWebhookPass)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestHealth(t *testing.T) {
	srv := newServer(testConfig(t), http.DefaultClient, &mockStore{}, nil)

	req := httptest.NewRequestWithContext(t.Context(), "GET", "/health", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

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

func TestBasicAuth(t *testing.T) {
	tests := []struct {
		name       string
		user, pass string
		setAuth    bool
		want       int
	}{
		{"no credentials", "", "", false, http.StatusUnauthorized},
		{"wrong credentials", "wrong", "wrong", true, http.StatusUnauthorized},
		{"wrong password", testWebhookUser, "wrong", true, http.StatusUnauthorized},
		{"valid credentials", testWebhookUser, testWebhookPass, true, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newServer(testConfig(t), http.DefaultClient, &mockStore{}, nil)
			req := httptest.NewRequestWithContext(t.Context(), "POST", "/incoming_call", nil)
			if tt.setAuth {
				req.SetBasicAuth(tt.user, tt.pass)
			}
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, req)

			if w.Code != tt.want {
				t.Fatalf("expected %d, got %d", tt.want, w.Code)
			}
		})
	}
}

func incomingCall(t *testing.T, cfg Configuration) IncomingResponse {
	t.Helper()
	srv := newServer(cfg, http.DefaultClient, &mockStore{}, nil)
	req := httptest.NewRequestWithContext(t.Context(), "POST", "/incoming_call", nil)
	req.SetBasicAuth(cfg.WebhookUser, cfg.WebhookPass)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("expected JSON content type, got %q", ct)
	}
	var resp IncomingResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	return resp
}

func TestIncomingCall(t *testing.T) {
	cfg := testConfig(t)
	resp := incomingCall(t, cfg)

	if resp.Play != cfg.VoicemailAudio {
		t.Errorf("expected play=%q, got %q", cfg.VoicemailAudio, resp.Play)
	}
	if resp.Next.SilenceDetection != "no" {
		t.Errorf("expected silencedetection=no, got %q", resp.Next.SilenceDetection)
	}
}

func TestIncomingCallIncludesCredentialsInCallbackURL(t *testing.T) {
	resp := incomingCall(t, testConfig(t))

	expected := "https://webhookuser:webhookpass@example.com/voicemail"
	if resp.Next.Record != expected {
		t.Fatalf("expected callback URL %q, got %q", expected, resp.Next.Record)
	}
}

func TestIncomingCallEscapesCredentialsInCallbackURL(t *testing.T) {
	cfg := testConfig(t)
	cfg.WebhookPass = "p@ss:w/rd?"
	resp := incomingCall(t, cfg)

	expected := "https://webhookuser:p%40ss%3Aw%2Frd%3F@example.com/voicemail"
	if resp.Next.Record != expected {
		t.Fatalf("expected callback URL %q, got %q", expected, resp.Next.Record)
	}
	u, err := url.Parse(resp.Next.Record)
	if err != nil {
		t.Fatalf("callback URL does not parse: %v", err)
	}
	if pass, _ := u.User.Password(); pass != cfg.WebhookPass {
		t.Fatalf("expected password to round-trip, got %q", pass)
	}
}

func TestIncomingCallRejectsGET(t *testing.T) {
	srv := newServer(testConfig(t), http.DefaultClient, &mockStore{}, nil)

	req := httptest.NewRequestWithContext(t.Context(), "GET", "/incoming_call", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Fatal("expected non-200 for GET request")
	}
}

func TestVoicemailMissingFields(t *testing.T) {
	srv := newServer(testConfig(t), http.DefaultClient, &mockStore{}, nil)

	tests := []struct {
		name   string
		values url.Values
	}{
		{"missing both", url.Values{}},
		{"missing wav", url.Values{"from": {"+46123456"}}},
		{"missing from", url.Values{"wav": {"https://api.46elks.com/a1/recordings/r1.wav"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if w := postVoicemail(t, srv, tt.values); w.Code != http.StatusBadRequest {
				t.Errorf("expected 400, got %d", w.Code)
			}
		})
	}
}

func TestVoicemailRejectsUntrustedWAVURL(t *testing.T) {
	var contacted atomic.Bool
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contacted.Store(true)
	}))
	defer attacker.Close()

	store := &mockStore{}
	srv := newServer(testConfig(t), attacker.Client(), store, nil)

	tests := []string{
		attacker.URL + "/recording.wav",
		"http://api.46elks.com/a1/recordings/r1.wav",
		"https://api.46elks.com.evil.example/r1.wav",
		"https://api.46elks.com@evil.example/r1.wav",
		"https://user:pass@api.46elks.com/r1.wav",
		"://not a url",
	}
	for _, wav := range tests {
		t.Run(wav, func(t *testing.T) {
			w := postVoicemail(t, srv, url.Values{"from": {"+46701234567"}, "wav": {wav}})
			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400, got %d", w.Code)
			}
		})
	}
	if contacted.Load() {
		t.Error("untrusted WAV host must not be contacted")
	}
	if store.called {
		t.Error("nothing should be uploaded for untrusted WAV URLs")
	}
}

func TestVoicemailAcceptsElksWAVURL(t *testing.T) {
	srv := newServer(testConfig(t), http.DefaultClient, &mockStore{}, nil)
	if err := srv.validateWAVURL("https://api.46elks.com/a1/recordings/r1.wav"); err != nil {
		t.Fatalf("expected 46elks URL to be accepted, got %v", err)
	}
}

func assertPrivateUpload(t *testing.T, in *s3.PutObjectInput, wantBody []byte) {
	t.Helper()
	if *in.Bucket != "test-bucket" {
		t.Errorf("expected bucket=test-bucket, got %q", *in.Bucket)
	}
	if *in.ContentType != "audio/wav" {
		t.Errorf("expected content-type=audio/wav, got %q", *in.ContentType)
	}
	if in.ACL != "" {
		t.Errorf("expected recordings to be private, got ACL %q", in.ACL)
	}
	if !regexp.MustCompile(`^voicemail/\d{14}-[A-Z2-7]{26}\.wav$`).MatchString(*in.Key) {
		t.Errorf("unexpected object key %q", *in.Key)
	}
	uploadedBody, _ := io.ReadAll(in.Body)
	if string(uploadedBody) != string(wantBody) {
		t.Errorf("uploaded data mismatch: got %q", uploadedBody)
	}
}

func TestVoicemailSuccess(t *testing.T) {
	wavData := []byte("fake-wav-data")
	tr := &mockTranscriber{text: "Hello, this is a test message."}
	f := newVoicemailFixture(t, newElksServer(t, wavData), tr)

	if w := f.post(t, f.defaultForm()); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !f.store.called {
		t.Fatal("expected S3 upload to be called")
	}
	assertPrivateUpload(t, f.store.input, wavData)
	if !tr.called {
		t.Fatal("expected transcriber to be called")
	}

	// Verify Slack message contains caller, recording link, and transcription
	p := f.slack.only(t)
	if p.Channel != "#test" {
		t.Errorf("expected Slack channel=#test, got %q", p.Channel)
	}
	name := strings.TrimPrefix(*f.store.input.Key, "voicemail/")
	for _, want := range []string{
		"+46701234567",
		"<https://example.com/recordings/" + name + ">",
		"\n>Hello, this is a test message.",
	} {
		if !strings.Contains(p.Text, want) {
			t.Errorf("expected Slack message to contain %q, got %q", want, p.Text)
		}
	}
}

func TestVoicemailRespondsBeforeNotifying(t *testing.T) {
	release := make(chan struct{})
	var slackDone atomic.Bool
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Time out rather than deadlock if the handler waits for Slack.
		select {
		case <-release:
		case <-time.After(2 * time.Second):
		}
		slackDone.Store(true)
	}))
	defer slack.Close()

	elks := newElksServer(t, []byte("fake-wav"))
	cfg := testConfig(t)
	cfg.SlackWebHookURL = slack.URL
	srv := newServer(cfg, elks.Client(), &mockStore{}, nil)
	srv.wavOrigin = mustParseURL(t, elks.URL)

	w := postVoicemail(t, srv, url.Values{"from": {"+46701234567"}, "wav": {elks.URL + "/r.wav"}})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 while Slack is still pending, got %d", w.Code)
	}
	if slackDone.Load() {
		t.Error("voicemail handler waited for the Slack notification before responding")
	}

	close(release)
	if err := srv.waitForBackground(t.Context()); err != nil {
		t.Fatalf("waiting for background work: %v", err)
	}
}

func TestVoicemailNoTranscriber(t *testing.T) {
	f := newVoicemailFixture(t, newElksServer(t, []byte("fake-wav-data")), nil)

	if w := f.post(t, f.defaultForm()); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	if p := f.slack.only(t); strings.Contains(p.Text, "\n>") {
		t.Errorf("expected no transcription quote, got %q", p.Text)
	}
}

func TestVoicemailTranscriptionFailureStillSucceeds(t *testing.T) {
	tr := &mockTranscriber{err: errors.New("whisper API error")}
	f := newVoicemailFixture(t, newElksServer(t, []byte("fake-wav-data")), tr)

	if w := f.post(t, f.defaultForm()); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !f.store.called {
		t.Fatal("expected S3 upload to be called")
	}
	if p := f.slack.only(t); strings.Contains(p.Text, "\n>") {
		t.Errorf("expected no transcription quote on failure, got %q", p.Text)
	}
}

func TestVoicemailSkipsTranscriptionForLargeRecordings(t *testing.T) {
	tr := &mockTranscriber{text: "should not be used"}
	f := newVoicemailFixture(t, newElksServer(t, make([]byte, maxTranscriptionSize+1)), tr)

	if w := f.post(t, f.defaultForm()); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if tr.called {
		t.Error("transcriber should not be called for recordings over the Whisper limit")
	}
	f.slack.only(t)
}

func TestVoicemailTooLarge(t *testing.T) {
	f := newVoicemailFixture(t, newElksServer(t, make([]byte, maxWAVSize+1)), nil)

	if w := f.post(t, f.defaultForm()); w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for oversized WAV, got %d", w.Code)
	}
	if f.store.called {
		t.Fatal("S3 upload should not be called for oversized files")
	}
}

func TestVoicemailElksErrorStatus(t *testing.T) {
	elks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer elks.Close()
	f := newVoicemailFixture(t, elks, nil)

	if w := f.post(t, f.defaultForm()); w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 when 46elks returns an error, got %d", w.Code)
	}
	if f.store.called {
		t.Fatal("error responses from 46elks must not be uploaded")
	}
}

func TestVoicemailS3Failure(t *testing.T) {
	f := newVoicemailFixture(t, newElksServer(t, []byte("fake-wav")), nil)
	f.store.err = io.ErrUnexpectedEOF

	if w := f.post(t, f.defaultForm()); w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on S3 failure, got %d", w.Code)
	}
}

func TestVoicemailSlackFailureStillSucceeds(t *testing.T) {
	f := newVoicemailFixture(t, newElksServer(t, []byte("fake-wav")), nil)
	f.srv.config.SlackWebHookURL = "http://127.0.0.1:1"

	if w := f.post(t, f.defaultForm()); w.Code != http.StatusOK {
		t.Fatalf("expected 200 even when Slack fails, got %d", w.Code)
	}
	if !f.store.called {
		t.Fatal("expected S3 upload to still be called")
	}
}

func TestPostToSlackReturnsErrorOnNon2xx(t *testing.T) {
	slack := newSlackServer(t, http.StatusForbidden)
	cfg := testConfig(t)
	cfg.SlackWebHookURL = slack.URL
	srv := newServer(cfg, slack.Client(), &mockStore{}, nil)

	if err := srv.postToSlack(t.Context(), "hello"); err == nil {
		t.Fatal("expected error for non-2xx Slack response")
	}
}

func TestSlackMessage(t *testing.T) {
	tests := []struct {
		name, from, transcription, want string
	}{
		{
			name: "no transcription",
			from: "+46701234567",
			want: "New voice message from +46701234567 <https://example.com/r.wav>!",
		},
		{
			name:          "escapes control characters",
			from:          "<!channel>",
			transcription: "Call <https://evil|here> & win",
			want:          "New voice message from &lt;!channel&gt; <https://example.com/r.wav>!\n>Call &lt;https://evil|here&gt; &amp; win",
		},
		{
			name:          "quotes every line",
			from:          "+46701234567",
			transcription: " first line\nsecond line\n",
			want:          "New voice message from +46701234567 <https://example.com/r.wav>!\n>first line\n>second line",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := slackMessage(tt.from, "https://example.com/r.wav", tt.transcription); got != tt.want {
				t.Errorf("got  %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestRecordingServesObject(t *testing.T) {
	name := "20260101120000-ABCDEFGHIJKLMNOPQRSTUVWXYZ.wav"
	store := &mockStore{objects: map[string][]byte{"voicemail/" + name: []byte("wav-bytes")}}
	srv := newServer(testConfig(t), http.DefaultClient, store, nil)

	req := httptest.NewRequestWithContext(t.Context(), "GET", "/recordings/"+name, nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "audio/wav" {
		t.Errorf("expected audio/wav, got %q", ct)
	}
	if cl := w.Header().Get("Content-Length"); cl != "9" {
		t.Errorf("expected Content-Length 9, got %q", cl)
	}
	if w.Body.String() != "wav-bytes" {
		t.Errorf("unexpected body %q", w.Body.String())
	}
}

func TestRecordingNotFound(t *testing.T) {
	store := &mockStore{}
	srv := newServer(testConfig(t), http.DefaultClient, store, nil)

	tests := []struct {
		name      string
		path      string
		wantFetch bool
	}{
		{"missing object", "/recordings/20260101120000-ABCDEFGHIJKLMNOPQRSTUVWXYZ.wav", true},
		{"invalid name", "/recordings/secret.wav", false},
		{"short id", "/recordings/20260101120000-abcd1234.wav", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store.gets = 0
			req := httptest.NewRequestWithContext(t.Context(), "GET", tt.path, nil)
			w := httptest.NewRecorder()
			srv.ServeHTTP(w, req)

			if w.Code != http.StatusNotFound {
				t.Fatalf("expected 404, got %d", w.Code)
			}
			if fetched := store.gets > 0; fetched != tt.wantFetch {
				t.Errorf("expected fetch=%v, got %v", tt.wantFetch, fetched)
			}
		})
	}
}

func validEnv() map[string]string {
	return map[string]string{
		"ELKS_USERNAME":     "elks",
		"ELKS_PASSWORD":     "elkspass",
		"S3_ACCESS_KEY":     "access",
		"S3_SECRET_KEY":     "secret",
		"S3_BUCKET_NAME":    "bucket",
		"S3_ENDPOINT":       "https://s3.example.com",
		"S3_REGION":         "fsn1",
		"SLACK_WEBHOOK_URL": "https://hooks.slack.com/x",
		"HOST":              "https://telefonista.example.com",
		"VOICEMAIL_AUDIO":   "https://example.com/greeting.mp3",
		"WEBHOOK_USER":      "user",
		"WEBHOOK_PASS":      "pass",
	}
}

func TestLoadConfig(t *testing.T) {
	env := validEnv()
	env["OPENAI_API_KEY"] = "sk-test"
	cfg, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Host.String() != "https://telefonista.example.com" {
		t.Errorf("unexpected host %q", cfg.Host)
	}
	if cfg.OpenAIAPIKey != "sk-test" || cfg.ElksPassword != "elkspass" || cfg.VoicemailAudio != env["VOICEMAIL_AUDIO"] {
		t.Errorf("config not populated: %+v", cfg)
	}
	if cfg.Port != "3000" {
		t.Errorf("expected default port 3000, got %q", cfg.Port)
	}
}

func TestLoadConfigReportsAllMissingVariables(t *testing.T) {
	env := validEnv()
	delete(env, "HOST")
	delete(env, "ELKS_PASSWORD")
	delete(env, "S3_SECRET_KEY")

	_, err := loadConfig(func(k string) string { return env[k] })
	if err == nil {
		t.Fatal("expected error")
	}
	for _, name := range []string{"HOST", "ELKS_PASSWORD", "S3_SECRET_KEY"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("expected error to mention %s, got %q", name, err)
		}
	}
}

func TestLoadConfigRejectsInvalidHost(t *testing.T) {
	for _, host := range []string{"example.com", "ftp://example.com", "https://"} {
		t.Run(host, func(t *testing.T) {
			env := validEnv()
			env["HOST"] = host
			if _, err := loadConfig(func(k string) string { return env[k] }); err == nil {
				t.Fatalf("expected error for HOST=%q", host)
			}
		})
	}
}

// whisperRequest is what the fake Whisper API received.
type whisperRequest struct {
	auth, model, filename string
	audio                 []byte
}

func newWhisperServer(t *testing.T, got *whisperRequest) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/transcriptions" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		got.auth = r.Header.Get("Authorization")
		mr, err := r.MultipartReader()
		if err != nil {
			t.Errorf("reading multipart: %v", err)
			return
		}
		for part, err := mr.NextPart(); !errors.Is(err, io.EOF); part, err = mr.NextPart() {
			if err != nil {
				t.Errorf("reading multipart: %v", err)
				return
			}
			data, _ := io.ReadAll(part)
			switch part.FormName() {
			case "model":
				got.model = string(data)
			case "file":
				got.filename = part.FileName()
				got.audio = data
			}
		}
		_, _ = w.Write([]byte(`{"text":"hej hej"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestWhisperTranscriber(t *testing.T) {
	var got whisperRequest
	api := newWhisperServer(t, &got)

	wt := newWhisperTranscriber("sk-test")
	wt.baseURL = api.URL
	text, err := wt.Transcribe(t.Context(), []byte("audio"), "voicemail.wav")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "hej hej" {
		t.Errorf("expected transcription %q, got %q", "hej hej", text)
	}
	if got.auth != "Bearer sk-test" {
		t.Errorf("unexpected Authorization header %q", got.auth)
	}
	if got.model != "whisper-1" {
		t.Errorf("unexpected model %q", got.model)
	}
	if got.filename != "voicemail.wav" || string(got.audio) != "audio" {
		t.Errorf("unexpected file part %q: %q", got.filename, got.audio)
	}
}

func TestWhisperTranscriberErrorStatus(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"bad key"}`, http.StatusUnauthorized)
	}))
	defer api.Close()

	wt := newWhisperTranscriber("sk-test")
	wt.baseURL = api.URL
	_, err := wt.Transcribe(t.Context(), []byte("audio"), "voicemail.wav")
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected error mentioning 401, got %v", err)
	}
}
