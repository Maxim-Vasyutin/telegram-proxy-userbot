# Telegram Proxy Userbot

A transparent Go userbot that relays messages, replies, reactions, and edit/delete events between pairs of Telegram groups ("merchant" and "support"). Support agents work inside their own groups and never appear in the merchant groups — all communication is bridged transparently by the userbot account.

## Quick start

```bash
# 1. Copy and edit the configuration files
cp config.example.yaml config.yaml
cp .env.example .env
# Edit config.yaml (set real chat_ids, phone numbers, session paths)
# Edit .env (set TELEGRAM_API_ID, TELEGRAM_API_HASH, DATABASE_DSN)

# 2. Start the full stack
docker compose -f deploy/docker-compose.yml up -d
```

View logs:

```bash
docker compose -f deploy/docker-compose.yml logs -f userbot
```

Validate config without starting:

```bash
docker compose -f deploy/docker-compose.yml run --rm userbot ./userbot --check
```

## Database setup

Apply migrations with [goose](https://github.com/pressly/goose):

```bash
goose -dir migrations postgres "$DATABASE_DSN" up
```

Roll back the latest migration:

```bash
goose -dir migrations postgres "$DATABASE_DSN" down
```

## First-time authorization

Each Telegram userbot account must be authorized **once** before the
service can run unattended. The `--auth` flag drives an interactive flow
that asks for the login code (and the cloud / 2FA password if the
account has one), saves the resulting session to disk, and exits.

Prerequisites:

1. Real Telegram API credentials in `.env` — get them from
   <https://my.telegram.org/apps> and put them under `TELEGRAM_API_ID`
   and `TELEGRAM_API_HASH`.
2. The phone number you want to authorize is listed in `config.yaml`
   under `accounts:` with a writable `session_path`.
3. Postgres is up and migrations have been applied (see above) — the
   normal run path requires it, but `--auth` itself does **not** touch
   the database, so this step is only required before `up -d`.

Run the auth flow inside the userbot container with both `--rm` (so the
container is discarded after authorization) and `-it` (so stdin is
attached for the interactive prompts):

```bash
docker compose -f deploy/docker-compose.yml run --rm -it userbot \
  ./userbot --auth --phone +79001234567
```

What happens:

1. The userbot connects to Telegram and asks Telegram to send a login
   code to your phone (it arrives in the official Telegram app, **not**
   via SMS for accounts that already have an active session somewhere).
2. The CLI prompts:

   ```text
   Enter the login code Telegram sent to +79001234567:
   ```

   Type the 5-digit code and press Enter.

3. If the account has cloud (2FA) password protection, a second prompt
   appears with the input masked:

   ```text
   Enter the cloud (2FA) password for +79001234567:
   ```

4. On success the binary logs `auth completed` (JSON) and exits with
   code `0`. The session file is written to the path declared in
   `config.yaml` (typically `/var/lib/userbot/sessions/account_<phone>.session`),
   inside the `userbot_sessions` Docker volume.

5. If the session is already valid (e.g. you re-run `--auth` after a
   successful authorization), the binary logs `session already valid`
   and exits with code `0` without prompting.

Repeat the command for every `accounts:` entry in `config.yaml`, then
start the service normally:

```bash
docker compose -f deploy/docker-compose.yml up -d
```

Troubleshooting:

- **"phone not found in config.accounts"** — the value passed via
  `--phone` must match an `accounts[].phone` entry in `config.yaml`
  byte-for-byte (including the leading `+`).
- **Session got revoked** ("Terminate all other sessions" pressed on a
  phone) — delete the corresponding `*.session` file from
  `userbot_sessions` and re-run `--auth`.
- **Cannot run interactively from CI / non-TTY** — `--auth` requires a
  TTY for password masking. Run it manually on a workstation or VPS,
  then copy the resulting `*.session` files to the production machine.
