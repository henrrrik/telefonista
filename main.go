package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
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
	S3AccessKey  string
	S3SecretKey  string
	S3Region     string
	S3Endpoint   string
	S3BucketName string
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
		Record string `json:"record"`
	} `json:"next"`
}

type objectUploader interface {
	PutObject(ctx context.Context, input *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

func newMux(config Configuration, httpClient *http.Client, uploader objectUploader) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /incoming_call", func(w http.ResponseWriter, r *http.Request) {
		resp := IncomingResponse{
			Play: config.VoicemailAudio,
			Next: struct {
				Record string `json:"record"`
			}{
				Record: config.Host + "/voicemail",
			},
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("POST /voicemail", func(w http.ResponseWriter, r *http.Request) {
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
		req.SetBasicAuth(config.ElksUserName, config.ElksPassword)

		resp, err := httpClient.Do(req)
		if err != nil {
			slog.Error("failed to download WAV", "error", err, "url", wav)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()

		audio, err := io.ReadAll(resp.Body)
		if err != nil {
			slog.Error("failed to read WAV body", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// Upload to Hetzner Object Storage
		path := "voicemail/" + time.Now().Format("20060102150405") + ".wav"
		slog.Info("uploading to object storage", "bucket", config.S3BucketName, "path", path)

		_, err = uploader.PutObject(r.Context(), &s3.PutObjectInput{
			Bucket:      aws.String(config.S3BucketName),
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

		// Post to Slack
		s3URL := fmt.Sprintf("%s/%s/%s", config.S3Endpoint, config.S3BucketName, path)
		slog.Info("posting to Slack", "channel", config.SlackChannel)

		payload := SlackPayload{
			UserName: config.SlackName,
			IconURL:  config.SlackIconURL,
			Text:     "New voice message from " + from + " <" + s3URL + ">!",
			Channel:  config.SlackChannel,
		}

		jsonPayload, err := json.Marshal(payload)
		if err != nil {
			slog.Error("failed to marshal Slack payload", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		slackReq, err := http.NewRequestWithContext(r.Context(), "POST", config.SlackWebHookURL, bytes.NewReader(jsonPayload))
		if err != nil {
			slog.Error("failed to create Slack request", "error", err)
			// Voicemail is saved, don't fail the response
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			return
		}
		slackReq.Header.Set("Content-Type", "application/json")

		slackResp, err := httpClient.Do(slackReq)
		if err != nil {
			slog.Error("failed to post to Slack", "error", err)
			// Voicemail is saved, don't fail the response
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			return
		}
		defer slackResp.Body.Close()

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	})

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
	}

	if config.S3Region == "" {
		config.S3Region = "eu-central"
	}
	if config.S3Endpoint == "" {
		config.S3Endpoint = "https://fsn1.your-objectstorage.com"
	}

	if config.S3BucketName == "" || config.ElksUserName == "" || config.SlackWebHookURL == "" {
		slog.Error("missing required environment variables: S3_BUCKET_NAME, ELKS_USERNAME, SLACK_WEBHOOK_URL")
		os.Exit(1)
	}

	s3Client := s3.New(s3.Options{
		Region:       config.S3Region,
		BaseEndpoint: aws.String(config.S3Endpoint),
		Credentials:  credentials.NewStaticCredentialsProvider(config.S3AccessKey, config.S3SecretKey, ""),
	})

	httpClient := &http.Client{Timeout: 20 * time.Second}
	mux := newMux(config, httpClient, s3Client)

	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	slog.Info("starting server", "port", port)
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("server failed", "error", err)
		os.Exit(1)
	}
}
