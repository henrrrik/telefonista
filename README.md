# Telefonista

![](https://upload.wikimedia.org/wikipedia/commons/0/0c/Bureau_téléphonique_parisien_vers_1900.jpg)

Telefonista is a simple voicemail service that sends recorded messages to a
Slack channel (or user). It uses the [46elks](https://46elks.com/) telephony
API and stores recordings in S3-compatible object storage (e.g. Hetzner).

## Environment variables

| Variable | Description | Required |
|---|---|---|
| `ELKS_USERNAME` | 46elks API username | Yes |
| `ELKS_PASSWORD` | 46elks API password | Yes |
| `S3_ACCESS_KEY` | Object storage access key | Yes |
| `S3_SECRET_KEY` | Object storage secret key | Yes |
| `S3_BUCKET_NAME` | Bucket name for storing recordings | Yes |
| `S3_ENDPOINT` | S3-compatible endpoint URL (default: `https://fsn1.your-objectstorage.com`) | No |
| `S3_REGION` | Storage region (default: `eu-central`) | No |
| `SLACK_WEBHOOK_URL` | Slack incoming webhook URL | Yes |
| `SLACK_CHANNEL` | Slack channel to post to | No |
| `SLACK_NAME` | Bot display name in Slack | No |
| `SLACK_ICON_URL` | Bot icon URL in Slack | No |
| `HOST` | Public URL of this service (used for 46elks callbacks) | No |
| `VOICEMAIL_AUDIO` | URL of audio file to play to callers | No |
| `PORT` | HTTP port (default: `3000`) | No |

## Running

```sh
go build -o telefonista
./telefonista
```

## License

Telefonista is released under the [MIT License](http://www.opensource.org/licenses/MIT).
