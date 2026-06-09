# Vintage Story MOTD changer

This app serves a small Discord-auth-protected admin UI and runs a daily scheduled update against a Vintage Story server config endpoint.

## What it does

On each scheduled run, the app:

1. Sends a `GET` request to the configured `serverconfig.json` endpoint.
2. Replaces only the `WelcomeMessage` field with a random message from the saved UI list.
3. Sends the full updated JSON back with a `PUT` request to the same endpoint.

## Environment variables

Create the following before starting the app:

```powershell
$env:MOTD_LISTEN_ADDR=":8080"
$env:MOTD_REMOTE_ENDPOINT="https://example.com:50000/v1/vintage/serverconfig.json"
$env:MOTD_REMOTE_API_KEY="your-api-key"
$env:MOTD_REMOTE_API_SECRET="your-api-secret"
$env:MOTD_DISCORD_CLIENT_ID="your-discord-client-id"
$env:MOTD_DISCORD_CLIENT_SECRET="your-discord-client-secret"
$env:MOTD_DISCORD_REDIRECT_URL="http://localhost:8080/auth/discord/callback"
$env:MOTD_SESSION_SECRET="replace-with-a-long-random-secret"
$env:MOTD_ALLOWED_DISCORD_IDS="123456789012345678,234567890123456789"
```

You can also copy `.env.example` to `.env` and fill in the values there for Docker or local env loading tools.

## Run locally

```powershell
go mod tidy
go run .
```

Then visit `http://localhost:8080`.

## Run with Docker

1. Copy `.env.example` to `.env` and fill in your real values.
2. Build and start the container:

```powershell
docker compose up --build -d
```

The app will be available at `http://localhost:8080`, and the persisted schedule/message data will be stored in the local `data/` folder through the bind mount in `compose.yaml`.

## GitHub release builds

This repo includes a GitHub Actions workflow at `.github/workflows/release-container.yml`.

When a GitHub release is published, the workflow will:

1. Build the Docker image from `Dockerfile`
2. Tag it from the release tag
3. Push it to GitHub Container Registry as `ghcr.io/<owner>/<repo>`

## Persisted settings

The schedule, timezone, message list, and last-run metadata are stored in:

`data/settings.json`
