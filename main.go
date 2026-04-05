package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type Configuration struct {
	SlackName       string
	SlackIconURL    string
	SlackWebHookURL string
	SlackChannel    string
	Host            string
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
}

type SlackPayload struct {
	UserName string `json:"username"`
	IconURL  string `json:"icon_url"`
	Text     string `json:"text"`
	Channel  string `json:"channel"`
}

type IncomingResponse struct {
	Play string `json:"play"`
	Next struct {
		Record           string `json:"record"`
		SilenceDetection string `json:"silencedetection"`
	} `json:"next"`
}

type objectUploader interface {
	PutObject(ctx context.Context, input *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

type transcriber interface {
	Transcribe(ctx context.Context, audio []byte, filename string) (string, error)
}

type whisperTranscriber struct {
	apiKey string
	client *http.Client
}

func (w *whisperTranscriber) Transcribe(ctx context.Context, audio []byte, filename string) (string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return "", fmt.Errorf("creating form file: %w", err)
	}
	if _, err := part.Write(audio); err != nil {
		return "", fmt.Errorf("writing audio data: %w", err)
	}
	if err := writer.WriteField("model", "whisper-1"); err != nil {
		return "", fmt.Errorf("writing model field: %w", err)
	}
	writer.Close()

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.openai.com/v1/audio/transcriptions", &body)
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+w.apiKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := w.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
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

func (c Configuration) webhookURL() string {
	return strings.Replace(c.Host, "://", "://"+c.WebhookUser+":"+c.WebhookPass+"@", 1)
}

func checkAuth(config Configuration, w http.ResponseWriter, r *http.Request) bool {
	user, pass, ok := r.BasicAuth()
	if !ok || user != config.WebhookUser || pass != config.WebhookPass {
		slog.Warn("rejected request with invalid credentials", "path", r.URL.Path, "remote", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

type server struct {
	config      Configuration
	httpClient  *http.Client
	uploader    objectUploader
	transcriber transcriber
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *server) handleIncomingCall(w http.ResponseWriter, r *http.Request) {
	if !checkAuth(s.config, w, r) {
		return
	}
	voicemailURL := s.config.webhookURL() + "/voicemail"
	resp := IncomingResponse{
		Play: s.config.VoicemailAudio,
		Next: struct {
			Record           string `json:"record"`
			SilenceDetection string `json:"silencedetection"`
		}{
			Record:           voicemailURL,
			SilenceDetection: "no",
		},
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(resp)
}

func (s *server) handleVoicemail(w http.ResponseWriter, r *http.Request) {
	if !checkAuth(s.config, w, r) {
		return
	}

	if err := r.ParseForm(); err != nil {
		slog.Error("failed to parse form", "error", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	from := r.FormValue("from")
	wav := r.FormValue("wav")
	if from == "" || wav == "" {
		slog.Warn("missing required form fields", "from", from, "wav", wav)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	slog.Info("voicemail received", "from", from)

	// Download WAV from 46elks
	slog.Info("downloading WAV", "url", wav)
	req, err := http.NewRequestWithContext(r.Context(), "GET", wav, nil)
	if err != nil {
		slog.Error("failed to create WAV request", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	req.SetBasicAuth(s.config.ElksUserName, s.config.ElksPassword)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		slog.Error("failed to download WAV", "error", err, "url", wav)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	const maxWAVSize = 50 * 1024 * 1024 // 50MB
	audio, err := io.ReadAll(io.LimitReader(resp.Body, maxWAVSize+1))
	if err != nil {
		slog.Error("failed to read WAV body", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if len(audio) > maxWAVSize {
		slog.Error("WAV file exceeds size limit", "size", len(audio), "max", maxWAVSize)
		http.Error(w, "recording too large", http.StatusBadRequest)
		return
	}

	// Upload to Hetzner Object Storage
	suffix := make([]byte, 4)
	rand.Read(suffix)
	path := "voicemail/" + time.Now().Format("20060102150405") + "-" + hex.EncodeToString(suffix) + ".wav"
	slog.Info("uploading to object storage", "bucket", s.config.S3BucketName, "path", path)

	_, err = s.uploader.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      aws.String(s.config.S3BucketName),
		Key:         aws.String(path),
		Body:        bytes.NewReader(audio),
		ContentType: aws.String("audio/wav"),
		ACL:         types.ObjectCannedACLPublicRead,
	})
	if err != nil {
		slog.Error("failed to upload to object storage", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Transcribe audio
	var transcription string
	if s.transcriber != nil {
		slog.Info("transcribing audio")
		transcription, err = s.transcriber.Transcribe(r.Context(), audio, "voicemail.wav")
		if err != nil {
			slog.Error("transcription failed", "error", err)
			// Continue without transcription — voicemail is saved
		}
	}

	// Post to Slack
	s3URL := fmt.Sprintf("%s/%s/%s", s.config.S3Endpoint, s.config.S3BucketName, path)
	slog.Info("posting to Slack", "channel", s.config.SlackChannel)

	slackText := "New voice message from " + from + " <" + s3URL + ">!"
	if transcription != "" {
		slackText += "\n>" + transcription
	}

	payload := SlackPayload{
		UserName: s.config.SlackName,
		IconURL:  s.config.SlackIconURL,
		Text:     slackText,
		Channel:  s.config.SlackChannel,
	}

	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		slog.Error("failed to marshal Slack payload", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	slackReq, err := http.NewRequestWithContext(r.Context(), "POST", s.config.SlackWebHookURL, bytes.NewReader(jsonPayload))
	if err != nil {
		slog.Error("failed to create Slack request", "error", err)
		// Voicemail is saved, don't fail the response
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		return
	}
	slackReq.Header.Set("Content-Type", "application/json")

	slackResp, err := s.httpClient.Do(slackReq)
	if err != nil {
		slog.Error("failed to post to Slack", "error", err)
		// Voicemail is saved, don't fail the response
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		return
	}
	defer slackResp.Body.Close()

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
}

func newMux(config Configuration, httpClient *http.Client, uploader objectUploader, t transcriber) *http.ServeMux {
	s := &server{config: config, httpClient: httpClient, uploader: uploader, transcriber: t}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("POST /incoming_call", s.handleIncomingCall)
	mux.HandleFunc("POST /voicemail", s.handleVoicemail)
	return mux
}

func main() {
	config := Configuration{
		SlackName:       os.Getenv("SLACK_NAME"),
		SlackIconURL:    os.Getenv("SLACK_ICON_URL"),
		SlackWebHookURL: os.Getenv("SLACK_WEBHOOK_URL"),
		SlackChannel:    os.Getenv("SLACK_CHANNEL"),
		Host:            os.Getenv("HOST"),
		VoicemailAudio:  os.Getenv("VOICEMAIL_AUDIO"),
		ElksUserName:    os.Getenv("ELKS_USERNAME"),
		ElksPassword:    os.Getenv("ELKS_PASSWORD"),
		S3AccessKey:     os.Getenv("S3_ACCESS_KEY"),
		S3SecretKey:     os.Getenv("S3_SECRET_KEY"),
		S3Region:        os.Getenv("S3_REGION"),
		S3Endpoint:      os.Getenv("S3_ENDPOINT"),
		S3BucketName:    os.Getenv("S3_BUCKET_NAME"),
		OpenAIAPIKey:    os.Getenv("OPENAI_API_KEY"),
		WebhookUser:     os.Getenv("WEBHOOK_USER"),
		WebhookPass:     os.Getenv("WEBHOOK_PASS"),
	}

	if config.S3BucketName == "" || config.S3Endpoint == "" || config.S3Region == "" || config.ElksUserName == "" || config.SlackWebHookURL == "" || config.WebhookUser == "" || config.WebhookPass == "" {
		slog.Error("missing required environment variables: S3_BUCKET_NAME, S3_ENDPOINT, S3_REGION, ELKS_USERNAME, SLACK_WEBHOOK_URL, WEBHOOK_USER, WEBHOOK_PASS")
		os.Exit(1)
	}

	s3Client := s3.New(s3.Options{
		Region:       config.S3Region,
		BaseEndpoint: aws.String(config.S3Endpoint),
		Credentials:  credentials.NewStaticCredentialsProvider(config.S3AccessKey, config.S3SecretKey, ""),
	})

	httpClient := &http.Client{Timeout: 20 * time.Second}

	var t transcriber
	if config.OpenAIAPIKey != "" {
		t = &whisperTranscriber{apiKey: config.OpenAIAPIKey, client: httpClient}
	}

	mux := newMux(config, httpClient, s3Client, t)

	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		slog.Info("starting server", "port", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server failed", "error", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	slog.Info("shutting down server")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("shutdown error", "error", err)
	}
}
