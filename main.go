package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const (
	maxWAVSize = 50 * 1024 * 1024 // 50MB
	// maxTranscriptionSize is the Whisper API's upload limit.
	maxTranscriptionSize = 25 * 1024 * 1024
	// backgroundTimeout bounds transcription and Slack notification for one voicemail.
	backgroundTimeout = 3 * time.Minute
	// recordingWriteTimeout replaces the server's WriteTimeout when streaming a recording.
	recordingWriteTimeout = 5 * time.Minute
	elksOrigin            = "https://api.46elks.com"
	recordingPrefix       = "voicemail/"
)

type Configuration struct {
	SlackName       string
	SlackIconURL    string
	SlackWebHookURL string
	SlackChannel    string
	Host            *url.URL
	VoicemailAudio  string
	ElksUserName    string
	ElksPassword    string
	S3AccessKey     string
	S3SecretKey     string
	S3Region        string
	S3Endpoint      string
	S3BucketName    string
	OpenAIAPIKey    string
	WebhookUser     string
	WebhookPass     string
	Port            string
}

// loadConfig reads the configuration from the environment using getenv.
func loadConfig(getenv func(string) string) (Configuration, error) {
	var c Configuration
	var host string
	vars := []struct {
		name     string
		dst      *string
		required bool
	}{
		{"SLACK_NAME", &c.SlackName, false},
		{"SLACK_ICON_URL", &c.SlackIconURL, false},
		{"SLACK_WEBHOOK_URL", &c.SlackWebHookURL, true},
		{"SLACK_CHANNEL", &c.SlackChannel, false},
		{"HOST", &host, true},
		{"VOICEMAIL_AUDIO", &c.VoicemailAudio, true},
		{"ELKS_USERNAME", &c.ElksUserName, true},
		{"ELKS_PASSWORD", &c.ElksPassword, true},
		{"S3_ACCESS_KEY", &c.S3AccessKey, true},
		{"S3_SECRET_KEY", &c.S3SecretKey, true},
		{"S3_REGION", &c.S3Region, true},
		{"S3_ENDPOINT", &c.S3Endpoint, true},
		{"S3_BUCKET_NAME", &c.S3BucketName, true},
		{"OPENAI_API_KEY", &c.OpenAIAPIKey, false},
		{"WEBHOOK_USER", &c.WebhookUser, true},
		{"WEBHOOK_PASS", &c.WebhookPass, true},
		{"PORT", &c.Port, false},
	}

	var missing []string
	for _, v := range vars {
		*v.dst = getenv(v.name)
		if v.required && *v.dst == "" {
			missing = append(missing, v.name)
		}
	}
	if len(missing) > 0 {
		return Configuration{}, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}

	u, err := url.Parse(host)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Configuration{}, fmt.Errorf("HOST must be an absolute http(s) URL, got %q", host)
	}
	c.Host = u

	if c.Port == "" {
		c.Port = "3000"
	}
	return c, nil
}

// webhookURL returns the public URL for path with the webhook credentials embedded,
// so that 46elks can authenticate its callbacks.
func (c Configuration) webhookURL(path string) string {
	u := c.Host.JoinPath(path)
	u.User = url.UserPassword(c.WebhookUser, c.WebhookPass)
	return u.String()
}

func (c Configuration) recordingURL(name string) string {
	return c.Host.JoinPath("recordings", name).String()
}

type SlackPayload struct {
	UserName string `json:"username"`
	IconURL  string `json:"icon_url"`
	Text     string `json:"text"`
	Channel  string `json:"channel"`
}

type nextAction struct {
	Record           string `json:"record"`
	SilenceDetection string `json:"silencedetection"`
}

type IncomingResponse struct {
	Play string     `json:"play"`
	Next nextAction `json:"next"`
}

type objectStore interface {
	PutObject(ctx context.Context, input *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(ctx context.Context, input *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

type transcriber interface {
	Transcribe(ctx context.Context, audio []byte, filename string) (string, error)
}

type whisperTranscriber struct {
	apiKey  string
	baseURL string
	client  *http.Client
}

func newWhisperTranscriber(apiKey string) *whisperTranscriber {
	return &whisperTranscriber{
		apiKey:  apiKey,
		baseURL: "https://api.openai.com/v1",
		client:  &http.Client{Timeout: 2 * time.Minute},
	}
}

func transcriptionForm(audio []byte, filename string) (*bytes.Buffer, string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return nil, "", fmt.Errorf("creating form file: %w", err)
	}
	if _, err := part.Write(audio); err != nil {
		return nil, "", fmt.Errorf("writing audio data: %w", err)
	}
	if err := writer.WriteField("model", "whisper-1"); err != nil {
		return nil, "", fmt.Errorf("writing model field: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, "", fmt.Errorf("closing multipart writer: %w", err)
	}
	return &body, writer.FormDataContentType(), nil
}

func (w *whisperTranscriber) Transcribe(ctx context.Context, audio []byte, filename string) (string, error) {
	body, contentType, err := transcriptionForm(audio, filename)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", w.baseURL+"/audio/transcriptions", body)
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+w.apiKey)
	req.Header.Set("Content-Type", contentType)

	resp, err := w.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("whisper API returned %d: %s", resp.StatusCode, respBody)
	}

	var result struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decoding response: %w", err)
	}
	return result.Text, nil
}

var (
	errWAVTooLarge = errors.New("WAV file exceeds size limit")

	// recordingNamePattern matches names produced by newRecordingName.
	recordingNamePattern = regexp.MustCompile(`^\d{14}-[A-Z2-7]{26}\.wav$`)

	slackEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
)

type server struct {
	config      Configuration
	httpClient  *http.Client
	store       objectStore
	transcriber transcriber
	// wavOrigin is the only origin recordings are downloaded from, since
	// requests to it carry the 46elks credentials.
	wavOrigin *url.URL
	mux       *http.ServeMux
	wg        sync.WaitGroup
}

func newServer(config Configuration, httpClient *http.Client, store objectStore, t transcriber) *server {
	origin, err := url.Parse(elksOrigin)
	if err != nil {
		panic(err)
	}
	s := &server{config: config, httpClient: httpClient, store: store, transcriber: t, wavOrigin: origin}
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("POST /incoming_call", s.handleIncomingCall)
	s.mux.HandleFunc("POST /voicemail", s.handleVoicemail)
	s.mux.HandleFunc("GET /recordings/{name}", s.handleRecording)
	return s
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// runInBackground runs fn after the request has been answered. Its context
// keeps the request's values but not its cancellation.
func (s *server) runInBackground(parent context.Context, fn func(context.Context)) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), backgroundTimeout)
	s.wg.Go(func() {
		defer cancel()
		fn(ctx)
	})
}

// waitForBackground blocks until all background work has finished or ctx is done.
func (s *server) waitForBackground(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *server) checkAuth(w http.ResponseWriter, r *http.Request) bool {
	user, pass, ok := r.BasicAuth()
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(s.config.WebhookUser)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(s.config.WebhookPass)) == 1
	if !ok || !userOK || !passOK {
		slog.Warn("rejected request with invalid credentials", "path", r.URL.Path, "remote", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("failed to write JSON response", "error", err)
	}
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *server) handleIncomingCall(w http.ResponseWriter, r *http.Request) {
	if !s.checkAuth(w, r) {
		return
	}
	writeJSON(w, IncomingResponse{
		Play: s.config.VoicemailAudio,
		Next: nextAction{
			Record:           s.config.webhookURL("voicemail"),
			SilenceDetection: "no",
		},
	})
}

func (s *server) validateWAVURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parsing WAV URL: %w", err)
	}
	if u.Scheme != s.wavOrigin.Scheme || u.Host != s.wavOrigin.Host || u.User != nil {
		return fmt.Errorf("WAV URL %q is not on %s", raw, s.wavOrigin)
	}
	return nil
}

func (s *server) parseVoicemailRequest(r *http.Request) (from, wavURL string, err error) {
	if err := r.ParseForm(); err != nil {
		return "", "", fmt.Errorf("parsing form: %w", err)
	}
	from = r.FormValue("from")
	wavURL = r.FormValue("wav")
	if from == "" || wavURL == "" {
		return "", "", fmt.Errorf("missing required form fields (from=%q, wav=%q)", from, wavURL)
	}
	if err := s.validateWAVURL(wavURL); err != nil {
		return "", "", err
	}
	return from, wavURL, nil
}

func (s *server) downloadWAV(ctx context.Context, wavURL string) ([]byte, error) {
	slog.Info("downloading WAV", "url", wavURL)
	// wavURL has been checked against wavOrigin by parseVoicemailRequest.
	req, err := http.NewRequestWithContext(ctx, "GET", wavURL, nil) //nolint:gosec // G704: origin validated
	if err != nil {
		return nil, fmt.Errorf("creating WAV request: %w", err)
	}
	req.SetBasicAuth(s.config.ElksUserName, s.config.ElksPassword)

	resp, err := s.httpClient.Do(req) //nolint:gosec // G704: origin validated
	if err != nil {
		return nil, fmt.Errorf("downloading WAV: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("downloading WAV: unexpected status %d", resp.StatusCode)
	}

	audio, err := io.ReadAll(io.LimitReader(resp.Body, maxWAVSize+1))
	if err != nil {
		return nil, fmt.Errorf("reading WAV body: %w", err)
	}
	if len(audio) > maxWAVSize {
		return nil, errWAVTooLarge
	}
	return audio, nil
}

func newRecordingName() string {
	return time.Now().Format("20060102150405") + "-" + rand.Text() + ".wav"
}

// storeRecording uploads audio as a private object and returns its recording name.
func (s *server) storeRecording(ctx context.Context, audio []byte) (string, error) {
	name := newRecordingName()
	key := recordingPrefix + name
	slog.Info("uploading to object storage", "bucket", s.config.S3BucketName, "path", key)

	_, err := s.store.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.config.S3BucketName),
		Key:         aws.String(key),
		Body:        bytes.NewReader(audio),
		ContentType: aws.String("audio/wav"),
	})
	if err != nil {
		return "", fmt.Errorf("uploading recording: %w", err)
	}
	return name, nil
}

// slackMessage builds the Slack notification text, escaping caller-controlled
// input so it cannot inject mentions or links.
func slackMessage(from, recordingURL, transcription string) string {
	msg := "New voice message from " + slackEscaper.Replace(from) + " <" + recordingURL + ">!"
	if transcription = strings.TrimSpace(transcription); transcription != "" {
		lines := strings.Split(slackEscaper.Replace(transcription), "\n")
		msg += "\n>" + strings.Join(lines, "\n>")
	}
	return msg
}

func (s *server) postToSlack(ctx context.Context, text string) error {
	slog.Info("posting to Slack", "channel", s.config.SlackChannel)

	jsonPayload, err := json.Marshal(SlackPayload{
		UserName: s.config.SlackName,
		IconURL:  s.config.SlackIconURL,
		Text:     text,
		Channel:  s.config.SlackChannel,
	})
	if err != nil {
		return fmt.Errorf("marshaling Slack payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", s.config.SlackWebHookURL, bytes.NewReader(jsonPayload))
	if err != nil {
		return fmt.Errorf("creating Slack request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("posting to Slack: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("slack returned %d: %s", resp.StatusCode, body)
	}
	return nil
}

// transcribe returns the transcription of audio, or "" if transcription is
// disabled, not possible, or fails.
func (s *server) transcribe(ctx context.Context, audio []byte) string {
	if s.transcriber == nil {
		return ""
	}
	if len(audio) > maxTranscriptionSize {
		slog.Warn("recording too large to transcribe", "bytes", len(audio))
		return ""
	}
	slog.Info("transcribing audio")
	text, err := s.transcriber.Transcribe(ctx, audio, "voicemail.wav")
	if err != nil {
		slog.Error("transcription failed", "error", err)
		return ""
	}
	return text
}

func (s *server) notify(ctx context.Context, from, name string, audio []byte) {
	text := slackMessage(from, s.config.recordingURL(name), s.transcribe(ctx, audio))
	if err := s.postToSlack(ctx, text); err != nil {
		slog.Error("failed to notify Slack", "error", err, "recording", name)
	}
}

func (s *server) handleVoicemail(w http.ResponseWriter, r *http.Request) {
	if !s.checkAuth(w, r) {
		return
	}

	from, wavURL, err := s.parseVoicemailRequest(r)
	if err != nil {
		slog.Warn("invalid voicemail request", "error", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	slog.Info("voicemail received", "from", from)

	audio, err := s.downloadWAV(r.Context(), wavURL)
	if errors.Is(err, errWAVTooLarge) {
		slog.Error("WAV file exceeds size limit", "url", wavURL)
		http.Error(w, "recording too large", http.StatusBadRequest)
		return
	} else if err != nil {
		slog.Error("failed to download WAV", "error", err, "url", wavURL)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	name, err := s.storeRecording(r.Context(), audio)
	if err != nil {
		slog.Error("failed to upload to object storage", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// The recording is safe; acknowledge 46elks now so slow transcription
	// can't time out the callback and trigger a retry.
	s.runInBackground(r.Context(), func(ctx context.Context) {
		s.notify(ctx, from, name, audio)
	})
}

// handleRecording streams a stored recording. The unguessable name is the
// capability, so links in Slack work without credentials.
func (s *server) handleRecording(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !recordingNamePattern.MatchString(name) {
		http.NotFound(w, r)
		return
	}

	out, err := s.store.GetObject(r.Context(), &s3.GetObjectInput{
		Bucket: aws.String(s.config.S3BucketName),
		Key:    aws.String(recordingPrefix + name),
	})
	var noSuchKey *types.NoSuchKey
	if errors.As(err, &noSuchKey) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		slog.Error("failed to fetch recording", "error", err, "recording", name)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer out.Body.Close()

	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if out.ContentLength != nil {
		w.Header().Set("Content-Length", strconv.FormatInt(*out.ContentLength, 10))
	}
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Now().Add(recordingWriteTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		slog.Warn("failed to extend write deadline", "error", err)
	}
	if _, err := io.Copy(w, out.Body); err != nil {
		slog.Warn("failed to stream recording", "error", err, "recording", name)
	}
}

func main() {
	if err := run(); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}

func run() error {
	config, err := loadConfig(os.Getenv)
	if err != nil {
		return err
	}

	s3Client := s3.New(s3.Options{
		Region:       config.S3Region,
		BaseEndpoint: aws.String(config.S3Endpoint),
		Credentials:  credentials.NewStaticCredentialsProvider(config.S3AccessKey, config.S3SecretKey, ""),
	})

	var t transcriber
	if config.OpenAIAPIKey != "" {
		t = newWhisperTranscriber(config.OpenAIAPIKey)
	}

	s := newServer(config, &http.Client{Timeout: 20 * time.Second}, s3Client, t)
	srv := &http.Server{
		Addr:              ":" + config.Port,
		Handler:           s,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		slog.Info("starting server", "port", config.Port)
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		return fmt.Errorf("server failed: %w", err)
	case <-ctx.Done():
	}

	slog.Info("shutting down server")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown error", "error", err)
	}
	if err := s.waitForBackground(shutdownCtx); err != nil {
		return fmt.Errorf("abandoning unfinished voicemail notifications: %w", err)
	}
	return nil
}
