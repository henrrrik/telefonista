# Telefonista

![](https://upload.wikimedia.org/wikipedia/commons/0/0c/Bureau_téléphonique_parisien_vers_1900.jpg)

Telefonista is a simple voicemail service that sends recorded messages to a
Slack channel (or user). It uses the [46elks](https://46elks.com/) telephony
API and stores recordings in S3-compatible object storage (e.g. Hetzner).
Optionally transcribes voicemails using OpenAI's Whisper API.


<img width="413" height="225" alt="telefonista-slack&#39;" src="https://github.com/user-attachments/assets/5bd8e624-f90a-45b6-9f42-907ade3de576" />

## What you need

- Somewhere to host the service (I use [Runway](https://runway.horse)) 🇪🇺
- S3-compatible object storage (I use [Hetzner](https://www.hetzner.com)) 🇪🇺
- [Slack](https://slack.com/) (you need to create a custom App with an incoming web hook enabled)
- An [46elks](https://46elks.se) 🇪🇺 account and a phone number with `voice_call` set to your `<hostname>/incoming_call?secret=<secret>`)
- An [OpenAI](https://platform.openai.com/) API key (if you want speech-to-text transcription)
- An audio file with your intro message (mp3, ogg or wav format)

## How it fits together

```mermaid
sequenceDiagram
      participant Caller as 📞 Caller
      participant 46elks
      participant Telefonista
      participant S3 as S3 Object Storage
      participant OpenAI as OpenAI Whisper
      participant Slack

      Caller->>46elks: Phone call
      46elks->>Telefonista: POST /incoming_call
      Telefonista-->>46elks: {play: voicemail audio, next: {record: /voicemail}}
      46elks->>Caller: Plays voicemail greeting
      Caller->>46elks: Leaves voice message
      46elks->>Telefonista: POST /voicemail (from, wav URL)
      Telefonista->>46elks: GET wav URL (Basic Auth)
      46elks-->>Telefonista: WAV audio data
      Telefonista->>S3: PutObject (voicemail/*.wav)
      S3-->>Telefonista: OK
      opt OpenAI Whisper API
          Telefonista->>OpenAI: POST /v1/audio/transcriptions (WAV)
          OpenAI-->>Telefonista: Transcription text
      end
      Telefonista->>Slack: POST webhook (message + S3 link + transcription)
      Slack-->>Telefonista: OK
      Telefonista-->>46elks: 200 OK
```

## Environment variables

| Variable | Description | Required |
|---|---|---|
| `ELKS_USERNAME` | 46elks API username | Yes |
| `ELKS_PASSWORD` | 46elks API password | Yes |
| `S3_ACCESS_KEY` | Object storage access key | Yes |
| `S3_SECRET_KEY` | Object storage secret key | Yes |
| `S3_BUCKET_NAME` | Bucket name for storing recordings | Yes |
| `S3_ENDPOINT` | S3-compatible endpoint URL | Yes |
| `S3_REGION` | Storage region | Yes |
| `OPENAI_API_KEY` | OpenAI API key for Whisper transcription (omit to disable) | No |
| `SLACK_WEBHOOK_URL` | Slack incoming webhook URL | Yes |
| `SLACK_CHANNEL` | Slack channel to post to | No |
| `SLACK_NAME` | Bot display name in Slack | No |
| `SLACK_ICON_URL` | Bot icon URL in Slack | No |
| `HOST` | Public URL of this service (used for 46elks callbacks) | Yes |
| `VOICEMAIL_AUDIO` | URL of audio file to play to callers | Yes |
| `WEBHOOK_SECRET` | Shared secret for authenticating 46elks webhooks (query param `?secret=`) | Yes |
| `PORT` | HTTP port (default: `3000`) | No |


## License

Telefonista is released under the [MIT License](http://www.opensource.org/licenses/MIT).
