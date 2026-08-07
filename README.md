# MatrixMigrate

A CLI tool for migrating from Mattermost to Matrix Synapse with multi-step, resumable migration support.

![MatrixMigrate TUI](img/ss-1.png)

## Features

- **Multi-step Migration**: Migrate users, teams, channels, and memberships in organized steps
- **SSH Tunnel Support**: Securely connect to remote servers via SSH port forwarding
- **Direct Mode**: Skip SSH entirely when the database and Matrix API are already reachable
- **Flexible SSH Authentication**: Support for both SSH key and password-based authentication
- **Auto-discovery**: Automatically reads Mattermost database credentials from `config.json`
- **Flexible Matrix Auth**: Login with username/password or use existing admin token
- **Beautiful TUI**: Interactive terminal UI powered by Bubble Tea with styled menus
- **Multi-language Support**: English (default) and Turkish interfaces
- **Detailed Connection Tests**: Step-by-step connection diagnostics for precise troubleshooting
- **Resumable**: Checkpoint-based migration that can be paused and resumed
- **Mapping Files**: Generates mapping files to track Mattermost → Matrix entity relationships
- **Application Service Support**: Import messages with original timestamps
- **Matrix Authentication Service (MAS)**: Create users via MAS so they can log in via SSO/OAuth

## Screenshots

### Main Menu
![Main Menu](img/ss-1.png)

### Connection Test
![Connection Test](img/ss-2.png)

## Installation

```bash
go install github.com/aligundogdu/matrixmigrate/cmd/matrixmigrate@latest
```

Or build from source:

```bash
git clone https://github.com/aligundogdu/matrixmigrate.git
cd matrixmigrate
make build
```

## Configuration

1. Copy the example configuration:
   ```bash
   cp config.example.yaml config.yaml
   ```

2. Edit `config.yaml` with your server details:

### Direct mode (no SSH)

Each side decides independently whether to use SSH, based on whether `ssh.host` is set.
Leave the `ssh` block empty and the tool connects directly instead of forwarding a port:

```yaml
mattermost:
  ssh: {}          # connect straight to the database
  database:        # required: with no SSH there is no remote config.json to read
    host: "127.0.0.1"
    port: 5432
    name: "mattermost"
    user: "mmuser"
    password_env: "MM_DB_PASSWORD"

matrix:
  ssh: {}          # talk to api.base_url instead of a tunnel
  api:
    base_url: "https://matrix.example.com"
    admin_token_env: "MATRIX_ADMIN_TOKEN"
  homeserver: "example.com"
```

Use it when:

- the homeserver sits behind an **HTTPS ingress** (Kubernetes/Helm, reverse proxy). There is
  no host where Synapse listens on `127.0.0.1:8008`, so there is nothing to tunnel to.
- matrixmigrate runs **on the server itself**, or against a **database dump restored
  locally** — which also keeps trial runs off the production database.

The two sides are independent: SSH into the Mattermost server while reaching Matrix over its
public URL is a valid combination.

Note that `mattermost.database` is not optional in direct mode. Credential auto-discovery
reads `config.json` over SSH, so with no SSH host it cannot run.

### SSH Key Authentication (Recommended)

```yaml
mattermost:
  ssh:
    host: "mattermost.example.com"
    user: "admin"
    key_path: "~/.ssh/id_rsa"
  config_path: "/opt/mattermost/config/config.json"

matrix:
  ssh:
    host: "matrix.example.com"
    user: "admin"
    key_path: "~/.ssh/id_rsa"
  auth:
    username: "admin"
    password_env: "MATRIX_ADMIN_PASSWORD"
  homeserver: "example.com"
```

### SSH Password Authentication

```yaml
mattermost:
  ssh:
    host: "mattermost.example.com"
    user: "root"
    password_env: "MM_SSH_PASSWORD"  # Uses environment variable
  config_path: "/opt/mattermost/config/config.json"

matrix:
  ssh:
    host: "matrix.example.com"
    user: "root"
    password_env: "MX_SSH_PASSWORD"
  auth:
    username: "admin"
    password_env: "MATRIX_ADMIN_PASSWORD"
  homeserver: "example.com"
```

3. Set environment variables:
   ```bash
   # For SSH password authentication
   export MM_SSH_PASSWORD="your-mattermost-ssh-password"
   export MX_SSH_PASSWORD="your-matrix-ssh-password"
   
   # For Matrix admin login
   export MATRIX_ADMIN_PASSWORD="your-admin-password"
   ```

### How It Works

**Mattermost**: The tool connects via SSH and reads `/opt/mattermost/config/config.json` to get database credentials. No manual database configuration needed!

**Matrix**: The tool logs in with username/password to get an access token. Alternatively, you can provide an existing admin token via `MATRIX_ADMIN_TOKEN` environment variable.

### Mattermost import options

Under `mattermost` in `config.yaml` you can set:

| Option | Default | Description |
|--------|---------|-------------|
| `ignored_users` | `[]` | List of Mattermost usernames to skip during import (case-insensitive). Useful for bot/service accounts. Ignored users are not created in Matrix, and their team/channel memberships are skipped. |

### File attachment options

Under `mattermost.files` in `config.yaml`:

| Option | Default | Description |
|--------|---------|-------------|
| `mode` | `link` | How message attachments are migrated. **`link`**: the message body gets a link back to the file on the Mattermost server (no file data is transferred). **`upload`**: the file is read from `local_data_path` and uploaded to the Matrix media repository, so attachments survive Mattermost being decommissioned. |
| `local_data_path` | — | Path to the Mattermost file storage directory, as reachable from the machine running the tool (typically an NFS/SSHFS mount of Mattermost's `data/` directory). Required when `mode: "upload"`. |
| `max_upload_size_mb` | `50` | Files larger than this are not uploaded. Must not exceed the Synapse `max_upload_size` setting, or uploads will be rejected by the homeserver. Rejected files are counted and reported at the end of the import. |
| `fallback_to_link_on_upload_failure` | `false` | When `mode: "upload"` and an individual upload fails, fall back to linking that file instead of recording an error. Useful for a first pass over a large archive where a handful of files are unreadable. |

### Matrix import options

Under `matrix.import` in `config.yaml` you can set:

| Option | Default | Description |
|--------|---------|-------------|
| `preserve_owner_and_alias` | `false` | Set room/space owner from Mattermost creator and set local alias (e.g. `#team-channel:domain`). Requires Application Service for room creation. |
| `force_join` | `false` | Add users to rooms/spaces via Synapse admin API (joined directly, no invite to accept). Use when users are already expected to be members. |
| **`public_room_join_rules`** | **`space_members`** | Who can join public (Mattermost) channels in Matrix. **`space_members`**: only members of the parent space/team can join (restricted join rule). **`public`**: anyone can join (default Matrix join rule). |
| `import_direct_messages` | `false` | Export and import Mattermost **direct message** channels (D type) as Matrix DMs. Rooms appear under **People** for both users. See [Direct messages import](#direct-messages-import) below. |
| `import_reactions` | `true` | Import emoji reactions as Matrix `m.reaction` annotations, after the messages they belong to. See [Reactions import](#reactions-import) below. |
| `space_visibility` | `invite_only` | Visibility of Matrix spaces created from Mattermost teams. **`invite_only`**: spaces are private (recommended; matches Mattermost team behaviour). **`public`**: spaces are publicly joinable. **`from_mattermost`**: derive per team from its type (`O` → public, `I` → invite-only). |
| `fallback_room_creator` | — | Matrix username (**localpart only**, e.g. `admin` for `@admin:domain`) used as room creator when the Mattermost channel has an empty `creator_id`, or when the real creator is a locked/deactivated account. If the user does not exist, the admin account from `auth.username` is used instead. Only meaningful with `preserve_owner_and_alias: true`. |
| `user_password.mode` | `auto` | How imported users get a password. **`auto`**: no password when `matrix.mas.enabled` is true (accounts are SSO-only), a random password otherwise. **`random`**: always generate a distinct random password per user. **`local_only`**: generate one only for users whose Mattermost account had no SSO provider (`auth_service` empty) — see [Mixed workspaces](#mixed-workspaces-mode-local_only). **`none`**: never set a password. |
| `user_password.length` | `24` | Length of generated passwords. Valid range 12–128. |
| `user_password.write_file` | `true` | Write generated passwords to `<assets_dir>/user-passwords-<timestamp>.csv` (mode `0600`) so they can be distributed. Set `false` to discard them. |

#### User passwords

Each imported user gets a distinct password generated with `crypto/rand`. There is no shared
or default password.

With `mode: "auto"` and MAS enabled — the common setup — **no password is set at all**. Those
accounts authenticate through SSO, so a local password would only widen the attack surface.

##### Mixed workspaces: `mode: "local_only"`

`auto` and `random` are all-or-nothing, which does not fit a workspace where most people sign
in through an upstream provider but a handful of accounts were created locally (for example
with `mmctl`). `auto` leaves those local accounts with no way in at all; `random` hands every
SSO user an unused second login and puts the whole workforce's credentials in one file.

`mode: "local_only"` generates a password only for users whose Mattermost account had no SSO
provider — that is, where `users.auth_service` is empty:

```yaml
matrix:
  import:
    user_password:
      mode: "local_only"
```

Two things to check before relying on it:

- **Password login must actually be enabled** on the homeserver, or in MAS
  (`passwords.enabled: true`). MAS answers `set-password` with `403 Password auth is
  disabled` otherwise, and that is logged at info level only — the run reports success while
  the affected accounts stay unreachable.
- **Re-run `export assets`.** `auth_service` is a field added to the exported asset JSON; an
  export taken with an older build does not carry it, and every user would then look local
  and be given a password.

When passwords *are* generated and `write_file` is true, they are written to
`data/assets/user-passwords-<timestamp>.csv`:

```csv
mattermost_username,matrix_user_id,password
alice_dev,@alice_dev:example.com,7Kq!mV2xRt9pLc4wZhY6sNbA
```

This file is plaintext credentials. It is created `0600`, is gitignored, and should be
deleted once the passwords have been distributed. If it is lost, affected users can only get
in via SSO or an admin-initiated password reset — the passwords are not stored anywhere else.

#### Email notifications

`matrixmigrate import enable-notifications` registers an email pusher for every migrated user
who has an address, so people are told by email about what they missed without having to find
the setting first.

Nothing does this by itself. Synapse only creates a pusher on its own registration path, which
never runs when accounts come from MAS — and even natively it is skipped for SSO logins
([synapse#10882](https://github.com/element-hq/synapse/issues/10882)) and ignored for accounts
created through the admin API
([#7135](https://github.com/matrix-org/synapse/issues/7135)).

- **Run it last.** A new pusher starts from the current stream position, so running it before
  the messages are in would email the entire import to everyone. The step refuses to run until
  `import_messages` has completed or been skipped.
- **Requires the Application Service.** The pusher has to be registered *as* the user, and
  `?user_id=` is the only way to do that.
- **Requires `email.enable_notifs` and `email.notif_from` on the homeserver.** Synapse only
  registers the `email` pusher type when notifications are configured; without it every call
  fails the same way, which the run reports as one server-side omission rather than hundreds of
  broken accounts.
- **Skipped users are counted by reason**: `no email address`, `user not mapped`,
  `account deactivated`, and `address not set on the account` — the last one meaning the
  address is missing from the account itself, which re-running `import assets` fixes.
- **Safe to re-run**: Synapse updates the existing pusher when `app_id` and `pushkey` match
  rather than adding a second one.

Push rules are left alone, so this does not change what notifies on desktop or mobile. Push
rules apply per user rather than per pusher, so restricting email to mentions and DMs would
also silence channel messages everywhere else. Synapse throttles the mails instead: one digest
per room, the first after `email.notif_delay_before_mail`, then a growing interval up to 24
hours, reset after 12 hours of quiet.

#### Accounts that already exist

An account can already exist before the import — most often because the person signed in
through SSO at some point, which creates them in MAS. Those are not created again; the import
records the mapping and moves on.

They do get their profile completed, additively:

- **Display name** is set only if the account has none. A name the person chose themselves, or
  one MAS imported from the upstream provider's claims, is left alone.
- **Email address** is appended to the account's existing third-party IDs. This matters: the
  admin API's `threepids` parameter *replaces* the whole list, so writing it blind would delete
  an address the person added themselves.
- With MAS, the address is also attached to the MAS account. MAS and Synapse keep separate
  address books — an address set through one is invisible to the other. Synapse's copy is what
  email notifications depend on; the MAS copy is what its account page shows and what account
  recovery uses. A MAS older than 0.15.0 has no route for this and is reported as a warning,
  not a failure.

Email addresses are lower-cased on the way in. Synapse canonicalises a pusher's push key but
stores the third-party ID exactly as given, so an address with capitals would later fail an
email pusher's ownership check with `THREEPID_NOT_FOUND` — leaving that person without email
notifications for no visible reason.

#### Direct messages import

When `import_direct_messages` is `true`:

- **Export**: In addition to public/private/group channels, the tool exports Mattermost **direct message** channels (type `D`). For D channels without a `creator_id`, the channel name is expected in the form `senderID_receiverID` (first ID = sender, second = receiver); both users are resolved from the user mapping and the room is created between them.
- **Import**: Each D channel is created as a **Matrix DM** (private room with `is_direct` set). The room is created as one of the two users (preferably not the Application Service bot) so it shows under **People** for both participants. The Application Service is used only when needed (e.g. to set `m.direct` account_data so clients list the room under People, and to send messages with original timestamps); the AS user is **not** added as a member of the DM.
- **Requirements**:
  - **Application Service** must be enabled (`matrix.appservice.enabled: true`) and `as_token_env` set. This is required to set `m.direct` for both users so the room appears under People for both, and for message import with correct senders and timestamps.
  - Both participants must exist in the user mapping (i.e. they must have been imported in the “Import assets” step).
- **Troubleshooting**:
  - If DMs are created but do not show under People for a user, ensure the Application Service token is valid and the AS registration allows the `user_id` query parameter for account_data.
  - Check logs for `ImportDirectChannelsAsDMs` and `CreateDirectRoom` to see which channels were skipped (e.g. missing user mapping, duplicate users, or API errors).
  - “Sender/receiver” from the Mattermost name format is used only when `creator_id` is empty; when `creator_id` is set, that user is used as the room creator and the other participant is taken from the channel name.

#### Reactions import

Reactions ride along with `import messages` as a second pass, once every message has an event
ID to point at. They are not a separate step.

- **Export**: `export messages` also reads the Mattermost `reactions` table. An installation
  without that table (Mattermost before 3.x) exports normally, with no reactions. **An export
  taken with an older version of this tool has no reactions in it** — re-run `export messages`
  or the import will find none.
- **Emoji**: shortcodes are resolved to Unicode from the bundled gemoji table. A **custom
  Mattermost emoji** has no Matrix equivalent and is imported as the literal text `:name:`,
  counted separately in the run summary as `custom_emoji`.
- **Requirements**: the **Application Service** token, as for messages. Without it reactions
  land as the admin account with the current timestamp instead of the original author's.
- **Skipped reactions** are counted by reason in the log rather than reported one by one. The
  reasons that matter:
  - `target message not imported` — the post itself never reached Matrix (a system message, a
    deleted post, or a failed send).
  - `sender left the channel` — Synapse refuses an event from a non-member even through the
    Application Service. These are **not** force-joined: putting someone back into a room they
    left to recover a thumbs-up is the wrong trade.
  - `user not mapped` — the reacting account was not imported (e.g. in `ignored_users`).
- **Re-runs are safe**: sent reactions are recorded in `message-mapping-<timestamp>.json` under
  `reactions`. Matrix does **not** deduplicate annotations server-side, so this record is the
  only thing preventing a second run from stacking a duplicate of every reaction.

## Usage

### Interactive Mode (TUI)

```bash
# Start with default language (English)
./matrixmigrate

# Start with Turkish interface
./matrixmigrate --lang tr
```

### Batch Mode

```bash
# Run specific steps
./matrixmigrate export assets
./matrixmigrate import assets
./matrixmigrate export memberships
./matrixmigrate import memberships
./matrixmigrate export messages
./matrixmigrate import messages

# Cleanup: admin leaves every migrated room and space
./matrixmigrate import leave-rooms

# Run with specific config
./matrixmigrate --config ./config.yaml export assets
```

#### Re-running membership import

Re-running `import memberships` re-applies the latest membership snapshot by default, so users
who joined a channel *after* the first import are added to the corresponding Matrix room.
Force-join treats an already-present user as success, so replaying is safe.

```bash
# Re-export from Mattermost first, so the snapshot includes recent joins
./matrixmigrate export memberships
./matrixmigrate import memberships

# Keep the old run-once behaviour: skip entirely if the step already completed
./matrixmigrate import memberships --skip-completed
```

### Test Connections

The connection test provides detailed step-by-step diagnostics:

```bash
# Test all connections
./matrixmigrate test all

# Test individual connections
./matrixmigrate test mattermost
./matrixmigrate test matrix
```

**Test Output Example:**
```
📋 Configuration
   ✓ Configuration file loaded (config.yaml found and parsed)
   ✓ Data directories accessible (Assets: ./data/assets, Mappings: ./data/mappings)

🗄️ Mattermost
   ✓ SSH configuration (Password auth via $MM_SSH_PASSWORD)
   ✓ SSH connection (root@mattermost.example.com:22)
   ✓ Mattermost config.json (/opt/mattermost/config/config.json)
   ✓ Database connection (150 users, 12 teams, 87 channels)

🔷 Matrix
   ✓ SSH configuration (Key: ~/.ssh/id_rsa)
   ✓ SSH connection (admin@matrix.example.com:22)
   ✓ API authentication (Login as admin via $MATRIX_ADMIN_PASSWORD)
   ✓ API connection (Homeserver: example.com)
   ⚠ Application Service (Not configured - message timestamps won't be preserved)

✓ All connection tests passed!
```

### Check Status

```bash
./matrixmigrate status
```

## Migration Steps

| Step | Command | Description |
|------|---------|-------------|
| 1a | `export assets` | Export users, teams, channels from Mattermost |
| 1b | `import assets` | Create users, spaces, rooms in Matrix |
| 2a | `export memberships` | Export team/channel memberships from Mattermost |
| 2b | `import memberships` | Apply memberships in Matrix |
| 3a | `export messages` | Export all messages from Mattermost |
| 3b | `import messages` | Import messages to Matrix rooms (requires Application Service for timestamps) |
| 4 | `import leave-rooms` | Optional cleanup: make the migration admin leave every migrated room and space |

### Leaving migrated rooms

The import steps already have the admin leave rooms as they go, but a failed leave is only
logged as a warning. With `force_join` and `import_direct_messages` enabled the admin enters
a great many rooms, so a few leftovers are normal — and each one means the migration admin
sits in a private room or someone else's direct message indefinitely.

```bash
./matrixmigrate import leave-rooms
```

It walks every room and space in the asset mapping, so it needs `import_assets` to have
completed but has no other dependency, and it is safe to repeat: a room the admin is not in
counts as already left rather than as a failure. Run it once at the end of a migration and
check the summary line for a non-zero failure count.

## Architecture

```
+-------------------------------------------------------------+
|                      Local Machine                          |
|  +--------------+  +----------+  +------------------------+ |
|  | MatrixMigrate|  |  Config  |  |      Data Store        | |
|  |     CLI      |--|   YAML   |  | - assets/*.json.gz     | |
|  +------+-------+  +----------+  | - mappings/*.json      | |
|         |                        | - state.json           | |
|         |                        +------------------------+ |
+---------+-------------------------------------------------------+
          |
    +-----+-----+
    |           |
    v           v
+----------+  +----------+
|Mattermost|  |  Matrix  |
|SSH (key/ |  |SSH (key/ |
| password)|  | password)|
|    |     |  |    |     |
|    v     |  |    v     |
|config.json  |   API    |
|    |     |  |    |     |
|    v     |  |    v     |
|PostgreSQL|  |Login/Token
+----------+  +----------+
```

## Mattermost → Matrix Mapping

| Mattermost | Matrix |
|------------|--------|
| Team | Space |
| Channel | Room |
| User | User |
| Team Membership | Space Membership |
| Channel Membership | Room Membership |
| `@username` | `@username:domain` (rendered as a pill, listed in `m.mentions.user_ids`) |
| `@all`, `@channel` | `@room` (text only — see below) |
| `@here` | unchanged — Matrix has no "whoever is online" concept |
| Reaction | `m.reaction` annotation |

### Mentions and notifications

Migrated messages are history, not news, so the import goes out of its way not to notify
anybody about them.

`@all` and `@channel` are rewritten to Matrix's `@room` so the archive reads correctly, but
**`m.mentions.room` is never set**. Declaring a real room mention would fire
`.m.rule.is_room_mention` for any sender above the `notifications.room` power level — and with
`preserve_owner_and_alias: true` the channel creator holds PL 100 — pinging everyone in the
room about an announcement from years ago.

For the same reason **every message this tool sends carries an `m.mentions` property, empty
when nobody is mentioned**. Per [MSC3952](https://github.com/matrix-org/matrix-spec-proposals/blob/main/proposals/3952-intentional-mentions.md)
the legacy push rules — `.m.rule.contains_display_name`, `.m.rule.contains_user_name` and
`.m.rule.roomnotif` — apply only while `m.mentions` is *missing*, and they match on the message
text rather than on any declared intent. Omitting the property would mean every migrated
message that merely contains someone's display name notifies them, including the filenames of
migrated attachments.

Genuine `@username` mentions still populate `m.mentions.user_ids`, so a real mention in the
history is attributed and rendered as a pill.

## Environment Variables

`config.yaml` never holds a secret directly — it stores the *name* of the environment variable
to read (`admin_token_env`, `password_env`, `as_token_env`, `client_secret_env`). Copy
[`.env.example`](.env.example) to `.env`, fill it in, and source it before running:

```bash
cp .env.example .env
set -a && . ./.env && set +a
./matrixmigrate export assets
```

| Variable | Description | Required |
|----------|-------------|----------|
| `MATRIX_ADMIN_PASSWORD` | Matrix admin password for login | Yes (if using auth) |
| `MATRIX_ADMIN_TOKEN` | Alternative: existing admin token | No |
| `MATRIX_AS_TOKEN` | Application Service token for message import | Yes (for messages) |
| `MATRIX_HS_TOKEN` | Homeserver token from the same Application Service registration | No |
| `MAS_CLIENT_ID` | MAS OAuth client ID (admin client) when `matrix.mas.enabled` is true | Yes (if using MAS) |
| `MAS_CLIENT_SECRET` | MAS OAuth client secret when `matrix.mas.enabled` is true | Yes (if using MAS) |
| `MM_SSH_PASSWORD` | Mattermost SSH password | No (if using key) |
| `MX_SSH_PASSWORD` | Matrix SSH password | No (if using key) |
| `SSH_KEY_PASSPHRASE` | SSH key passphrase (if encrypted) | No |

## Rate Limiting

If you're getting too many 429 (rate limit) errors during migration, you have two options:

### Option 1: Adjust Rate Limit Settings in Config

Add rate limiting configuration to your `config.yaml`:

```yaml
matrix:
  rate_limit:
    # Requests per second (lower = slower but safer, 0 = no limit)
    # Default: 5.0 (200ms between requests)
    requests_per_second: 2.0
    
    # Maximum retries when rate limited (429 error)
    # Default: 5
    max_retries: 10
    
    # Base delay in milliseconds for exponential backoff
    # Actual delay: base_delay * 2^retry_count (e.g., 2s, 4s, 8s, 16s, 32s)
    # Default: 2000 (2 seconds)
    retry_base_delay_ms: 3000
```

### Option 2: Temporarily Disable Rate Limiting on Synapse

Add this to your Synapse `homeserver.yaml`:

```yaml
rc_message:
  per_second: 10000
  burst_count: 10000
rc_registration:
  per_second: 10000
  burst_count: 10000
rc_login:
  address:
    per_second: 10000
    burst_count: 10000
  account:
    per_second: 10000
    burst_count: 10000
  failed_attempts:
    per_second: 10000
    burst_count: 10000
rc_admin_redaction:
  per_second: 10000
  burst_count: 10000
rc_joins:
  local:
    per_second: 10000
    burst_count: 10000
  remote:
    per_second: 10000
    burst_count: 10000
rc_invites:
  per_room:
    per_second: 10000
    burst_count: 10000
  per_user:
    per_second: 10000
    burst_count: 10000
rc_room_creation:
  per_second: 10000
  burst_count: 10000
```

**⚠️ Important:** Remember to restart Synapse (`systemctl restart matrix-synapse`) and **re-enable rate limiting** after the migration is complete for security!

### ⚠️ Important Notice About Rate Limiting

Even if you configure all rate limit bypass settings, **some items may still fail to import** due to rate limiting or temporary network issues. **Don't panic!**

The migration tool is designed to be **resumable**:
- Already imported users, spaces, and rooms are tracked in the mapping file
- When you run the import command again, it will **skip already imported items** and only process the failed ones
- Simply wait a few minutes and run the same import command again
- Repeat until all items are successfully imported

This is normal behavior and the tool will eventually complete all imports.

## Application Service Setup (for Message Import)

To import messages with their **original timestamps**, you need to configure an Application Service (AS) on your Synapse server. Without AS, messages will be imported with the current timestamp.

**Note:** The connection test will show a warning (⚠) if Application Service is not configured, reminding you that message timestamps won't be preserved.

### Step 1: Generate Tokens

```bash
# Generate AS token
openssl rand -hex 32
# Example output: a1b2c3d4e5f6...

# Generate HS token
openssl rand -hex 32
# Example output: 9z8y7x6w5v4u...
```

### Step 2: Create Registration File

Create a file on your Synapse server (e.g., `/etc/matrix-synapse/matrixmigrate.yaml`):

```yaml
id: matrixmigrate
url: null  # No callback URL needed - outbound only
as_token: "YOUR_GENERATED_AS_TOKEN"
hs_token: "YOUR_GENERATED_HS_TOKEN"
sender_localpart: matrixmigrate
rate_limited: false  # Disable rate limiting for AS
namespaces:
  users:
    - exclusive: false
      regex: "@.*:example\\.com"   # Replace example.com with your homeserver domain
  rooms:
    - exclusive: false
      regex: "!.*:example\\.com"
  aliases:
    - exclusive: true
      regex: "#.*:example\\.com"   # Required for room/space aliases when preserve_owner_and_alias is enabled
```

### Step 3: Register with Synapse

Add to your `homeserver.yaml`:

```yaml
app_service_config_files:
  - /etc/matrix-synapse/matrixmigrate.yaml
```

Then restart Synapse:

```bash
systemctl restart matrix-synapse
```

### Step 4: Configure MatrixMigrate

Add to your `config.yaml`:

```yaml
matrix:
  appservice:
    enabled: true
    as_token_env: "MATRIX_AS_TOKEN"
```

Set the environment variable:

```bash
export MATRIX_AS_TOKEN="YOUR_GENERATED_AS_TOKEN"
```

### Step 5: Import Messages

```bash
./matrixmigrate import messages
```

**Note:** The AS token allows the migration tool to send messages on behalf of users with their original timestamps. This is the only way to preserve message history accurately.

## Matrix Authentication Service (MAS)

If your Synapse server uses the **Matrix Authentication Service (MAS)** for SSO/OAuth, users created directly via the Synapse Admin API cannot be linked to MAS and will get "Localpart not available" when logging in. Enabling MAS in MatrixMigrate creates users through the MAS Admin API instead, so they can sign in via SSO.

### When to use MAS

- Your homeserver is configured to use MAS for authentication (OAuth/SSO).
- You see "Localpart not available" or similar errors when migrated users try to log in.
- You want migrated users to use the same SSO flow as existing users.

### Configuration

1. **Enable the Admin API in MAS**  
   In your MAS config, add the `adminapi` resource to an HTTP listener (see [MAS Admin API docs](https://matrix-org.github.io/matrix-authentication-service/topics/admin-api.html)).

2. **Create an admin OAuth client**  
   Register a client in MAS and add its `client_id` to `policy.data.admin_clients` so it can use the client_credentials grant with scope `urn:mas:admin`.

3. **Configure MatrixMigrate**  
   In `config.yaml`:

```yaml
matrix:
  # ... ssh, api, auth, homeserver ...
  mas:
    enabled: true
    endpoint: "http://mas.example.com:8080"   # or http://localhost:8080 if you tunnel MAS
    client_id_env: "MAS_CLIENT_ID"
    client_secret_env: "MAS_CLIENT_SECRET"
```

4. **Set environment variables**  
   Before running import:

```bash
export MAS_CLIENT_ID="your-mas-admin-client-id"
export MAS_CLIENT_SECRET="your-mas-admin-client-secret"
```

When MAS is enabled, **user import** (e.g. `import assets`) creates users via the MAS Admin API and sets a temporary password; all other steps (spaces, rooms, memberships, messages) still use the Synapse API as before.

### Two MAS settings that fail silently

Both of these let a migration finish and report success while leaving accounts nobody can log
into. Verify them on a single throwaway account **before** the real run, not after.

#### `claims_imports.localpart.on_conflict` must not be `fail`

Creating users ahead of time is the entire point of MAS support, but by default MAS refuses to
attach an upstream identity to a localpart that already exists — and `fail` is the default. The
migration would create every account correctly, and every SSO login afterwards would be
rejected. On the upstream provider:

```yaml
upstream_oauth2:
  providers:
    - # ...
      claims_imports:
        localpart:
          action: force
          on_conflict: add     # default is "fail"
```

`add` links the upstream account to the existing user; `set` does the same but only when no
other link exists for that provider. See
[Configure an upstream SSO provider](https://element-hq.github.io/matrix-authentication-service/setup/sso.html).

**Test it**: pre-create the localpart of one account you control that has never signed in,
then sign in with it via SSO.

```bash
TOKEN=$(curl -s -u "$MAS_CLIENT_ID:$MAS_CLIENT_SECRET" \
  -d grant_type=client_credentials -d scope=urn:mas:admin \
  "$MAS_URL/oauth2/token" | jq -r .access_token)

curl -s -X POST "$MAS_URL/api/admin/v1/users" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/vnd.api+json" \
  -d '{"username":"ssotest"}' | jq
```

#### `passwords.enabled` must be true if any password is generated

Only relevant when `user_password.mode` generates passwords at all (`random` or
`local_only`). If MAS is OIDC-only, `set-password` returns `403 Password auth is disabled`,
which the client logs at info level and carries on — so the generated-passwords CSV would
list credentials that were never set.

Password login and SSO are independent switches in MAS and can both be on; users then see a
password form alongside the provider button, instead of being redirected straight to the
provider.

**Test it**: set a password on a throwaway account and check the status code.

```bash
ULID=$(curl -s -X POST "$MAS_URL/api/admin/v1/users" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/vnd.api+json" \
  -d '{"username":"pwtest"}' | jq -r .data.id)

curl -s -o /dev/null -w 'HTTP %{http_code}\n' \
  -X POST "$MAS_URL/api/admin/v1/users/$ULID/set-password" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/vnd.api+json" \
  -d '{"password":"a-24-character-test-pass"}'
```

`200` is fine; `403` means the setting is missing. Then actually sign in with it, and remove
both test accounts afterwards (see the Swagger UI at `$MAS_URL/api/doc/`).

---

## Troubleshooting

Use `./matrixmigrate test all` to identify exactly where the connection fails.

### SSH Connection Failed
- For key auth: Ensure SSH key is properly configured and has correct permissions
- For password auth: Check that the password environment variable is set
- Verify the SSH port is correct (default: 22)

### Mattermost Config Not Found
- Check the `config_path` in your config.yaml
- Try different paths: `/opt/mattermost/config/config.json`, `/opt/mattermost/config.json`
- Ensure the SSH user has read access to the file

### Matrix Login Failed
- Verify the admin username and password
- Check if the Matrix homeserver supports password login
- Ensure the user has admin privileges

### Database Connection Failed
- The tool reads credentials from Mattermost's config.json automatically
- Ensure PostgreSQL is running and accessible from localhost on the Mattermost server

### Application Service Warning
- If you see "⚠ Application Service (Not configured)" in the connection test, this means:
  - Messages will be imported with current timestamps instead of original timestamps
  - To fix: Follow the "Application Service Setup" section above

## License

MIT License

---

# MatrixMigrate (Türkçe)

Mattermost'tan Matrix Synapse'a çok adımlı, devam ettirilebilir taşıma desteği sunan bir CLI aracı.

![MatrixMigrate TUI](img/ss-1.png)

## Özellikler

- **Çok Adımlı Taşıma**: Kullanıcıları, takımları, kanalları ve üyelikleri düzenli adımlarla taşıyın
- **SSH Tünel Desteği**: SSH port yönlendirme ile uzak sunuculara güvenli bağlantı
- **Esnek SSH Kimlik Doğrulama**: SSH anahtarı veya şifre tabanlı kimlik doğrulama desteği
- **Otomatik Keşif**: Mattermost veritabanı bilgilerini `config.json` dosyasından otomatik okur
- **Esnek Matrix Kimlik Doğrulama**: Kullanıcı adı/şifre ile giriş veya mevcut admin token kullanımı
- **Güzel TUI**: Bubble Tea ile geliştirilmiş, stilli menülere sahip etkileşimli terminal arayüzü
- **Çoklu Dil Desteği**: İngilizce (varsayılan) ve Türkçe arayüz
- **Detaylı Bağlantı Testleri**: Sorunları tam olarak belirlemek için adım adım bağlantı tanılama
- **Devam Ettirilebilir**: Duraklatılıp devam ettirilebilen kontrol noktası tabanlı taşıma
- **Eşleme Dosyaları**: Mattermost → Matrix varlık ilişkilerini izlemek için eşleme dosyaları oluşturur
- **Application Service Desteği**: Mesajları orijinal zaman damgalarıyla aktarın

## Ekran Görüntüleri

### Ana Menü
![Ana Menü](img/ss-1.png)

### Bağlantı Testi
![Bağlantı Testi](img/ss-2.png)

## Kurulum

```bash
go install github.com/aligundogdu/matrixmigrate/cmd/matrixmigrate@latest
```

Veya kaynaktan derleyin:

```bash
git clone https://github.com/aligundogdu/matrixmigrate.git
cd matrixmigrate
make build
```

## Yapılandırma

1. Örnek yapılandırmayı kopyalayın:
   ```bash
   cp config.example.yaml config.yaml
   ```

2. `config.yaml` dosyasını sunucu bilgilerinizle düzenleyin:

### SSH Anahtar Kimlik Doğrulaması (Önerilen)

```yaml
mattermost:
  ssh:
    host: "mattermost.example.com"
    user: "admin"
    key_path: "~/.ssh/id_rsa"
  config_path: "/opt/mattermost/config/config.json"

matrix:
  ssh:
    host: "matrix.example.com"
    user: "admin"
    key_path: "~/.ssh/id_rsa"
  auth:
    username: "admin"
    password_env: "MATRIX_ADMIN_PASSWORD"
  homeserver: "example.com"
```

### SSH Şifre Kimlik Doğrulaması

```yaml
mattermost:
  ssh:
    host: "mattermost.example.com"
    user: "root"
    password_env: "MM_SSH_PASSWORD"  # Ortam değişkeni kullanır
  config_path: "/opt/mattermost/config/config.json"

matrix:
  ssh:
    host: "matrix.example.com"
    user: "root"
    password_env: "MX_SSH_PASSWORD"
  auth:
    username: "admin"
    password_env: "MATRIX_ADMIN_PASSWORD"
  homeserver: "example.com"
```

3. Ortam değişkenlerini ayarlayın:
   ```bash
   # SSH şifre kimlik doğrulaması için
   export MM_SSH_PASSWORD="mattermost-ssh-sifreniz"
   export MX_SSH_PASSWORD="matrix-ssh-sifreniz"
   
   # Matrix admin girişi için
   export MATRIX_ADMIN_PASSWORD="admin-sifreniz"
   ```

### Nasıl Çalışır

**Mattermost**: Araç SSH ile bağlanır ve veritabanı bilgilerini almak için `/opt/mattermost/config/config.json` dosyasını okur. Manuel veritabanı yapılandırmasına gerek yok!

**Matrix**: Araç erişim token'ı almak için kullanıcı adı/şifre ile giriş yapar. Alternatif olarak, `MATRIX_ADMIN_TOKEN` ortam değişkeni ile mevcut bir admin token sağlayabilirsiniz.

## Kullanım

### Etkileşimli Mod (TUI)

```bash
# Varsayılan dil (İngilizce) ile başlat
./matrixmigrate

# Türkçe arayüz ile başlat
./matrixmigrate --lang tr
```

### Toplu İşlem Modu

```bash
# Belirli adımları çalıştır
./matrixmigrate export assets
./matrixmigrate import assets
./matrixmigrate export memberships
./matrixmigrate import memberships
./matrixmigrate export messages
./matrixmigrate import messages

# Belirli config ile çalıştır
./matrixmigrate --config ./config.yaml export assets
```

### Bağlantı Testi

Bağlantı testi detaylı adım adım tanılama sağlar:

```bash
# Tüm bağlantıları test et
./matrixmigrate test all

# Ayrı ayrı bağlantıları test et
./matrixmigrate test mattermost
./matrixmigrate test matrix
```

**Test Çıktısı Örneği:**
```
📋 Yapılandırma
   ✓ Yapılandırma dosyası yüklendi (config.yaml bulundu ve ayrıştırıldı)
   ✓ Veri dizinleri erişilebilir (Assets: ./data/assets, Mappings: ./data/mappings)

🗄️ Mattermost
   ✓ SSH yapılandırması ($MM_SSH_PASSWORD ile şifre doğrulama)
   ✓ SSH bağlantısı (root@mattermost.example.com:22)
   ✓ Mattermost config.json (/opt/mattermost/config/config.json)
   ✓ Veritabanı bağlantısı (150 kullanıcı, 12 takım, 87 kanal)

🔷 Matrix
   ✓ SSH yapılandırması (Anahtar: ~/.ssh/id_rsa)
   ✓ SSH bağlantısı (admin@matrix.example.com:22)
   ✓ API kimlik doğrulama ($MATRIX_ADMIN_PASSWORD ile admin olarak giriş)
   ✓ API bağlantısı (Homeserver: example.com)
   ⚠ Application Service (Yapılandırılmamış - mesaj zaman damgaları korunmayacak)

✓ Tüm bağlantı testleri başarılı!
```

### Durum Kontrolü

```bash
./matrixmigrate status
```

## Taşıma Adımları

| Adım | Komut | Açıklama |
|------|-------|----------|
| 1a | `export assets` | Mattermost'tan kullanıcıları, takımları, kanalları dışa aktar |
| 1b | `import assets` | Matrix'te kullanıcıları, space'leri, odaları oluştur |
| 2a | `export memberships` | Mattermost'tan takım/kanal üyeliklerini dışa aktar |
| 2b | `import memberships` | Matrix'te üyelikleri uygula |
| 3a | `export messages` | Mattermost'tan tüm mesajları dışa aktar |
| 3b | `import messages` | Mesajları Matrix odalarına aktar (zaman damgaları için Application Service gerektirir) |

## Mimari

```
+-------------------------------------------------------------+
|                      Yerel Makine                           |
|  +--------------+  +----------+  +------------------------+ |
|  | MatrixMigrate|  |  Config  |  |      Veri Deposu       | |
|  |     CLI      |--|   YAML   |  | - assets/*.json.gz     | |
|  +------+-------+  +----------+  | - mappings/*.json      | |
|         |                        | - state.json           | |
|         |                        +------------------------+ |
+---------+-------------------------------------------------------+
          |
    +-----+-----+
    |           |
    v           v
+----------+  +----------+
|Mattermost|  |  Matrix  |
|SSH(anahtar| |SSH(anahtar|
|  /şifre) |  |  /şifre) |
|    |     |  |    |     |
|    v     |  |    v     |
|config.json  |   API    |
|    |     |  |    |     |
|    v     |  |    v     |
|PostgreSQL|  |Giriş/Token
+----------+  +----------+
```

## Mattermost → Matrix Eşlemesi

| Mattermost | Matrix |
|------------|--------|
| Team | Space |
| Channel | Room |
| User | User |
| Team Membership | Space Membership |
| Channel Membership | Room Membership |

## Ortam Değişkenleri

| Değişken | Açıklama | Zorunlu |
|----------|----------|---------|
| `MATRIX_ADMIN_PASSWORD` | Giriş için Matrix admin şifresi | Evet (auth kullanılıyorsa) |
| `MATRIX_ADMIN_TOKEN` | Alternatif: mevcut admin token | Hayır |
| `MATRIX_AS_TOKEN` | Mesaj aktarımı için Application Service token | Evet (mesajlar için) |
| `MM_SSH_PASSWORD` | Mattermost SSH şifresi | Hayır (anahtar kullanılıyorsa) |
| `MX_SSH_PASSWORD` | Matrix SSH şifresi | Hayır (anahtar kullanılıyorsa) |
| `SSH_KEY_PASSPHRASE` | SSH anahtar parolası (şifreli ise) | Hayır |

## Hız Sınırlama (Rate Limiting)

Migrasyon sırasında çok fazla 429 (hız sınırı) hatası alıyorsanız, iki seçeneğiniz var:

### Seçenek 1: Config'de Hız Sınırı Ayarlarını Düzenleyin

`config.yaml` dosyanıza hız sınırlama yapılandırması ekleyin:

```yaml
matrix:
  rate_limit:
    # Saniyede istek sayısı (düşük = yavaş ama güvenli, 0 = sınırsız)
    # Varsayılan: 5.0 (istekler arası 200ms)
    requests_per_second: 2.0
    
    # Hız sınırı hatası (429) alındığında maksimum deneme sayısı
    # Varsayılan: 5
    max_retries: 10
    
    # Üstel geri çekilme için milisaniye cinsinden temel gecikme
    # Gerçek gecikme: temel_gecikme * 2^deneme_sayısı (örn. 2s, 4s, 8s, 16s, 32s)
    # Varsayılan: 2000 (2 saniye)
    retry_base_delay_ms: 3000
```

### Seçenek 2: Synapse'de Hız Sınırlamayı Geçici Olarak Devre Dışı Bırakın

Synapse `homeserver.yaml` dosyanıza şunu ekleyin:

```yaml
rc_message:
  per_second: 10000
  burst_count: 10000
rc_registration:
  per_second: 10000
  burst_count: 10000
rc_login:
  address:
    per_second: 10000
    burst_count: 10000
  account:
    per_second: 10000
    burst_count: 10000
  failed_attempts:
    per_second: 10000
    burst_count: 10000
rc_admin_redaction:
  per_second: 10000
  burst_count: 10000
rc_joins:
  local:
    per_second: 10000
    burst_count: 10000
  remote:
    per_second: 10000
    burst_count: 10000
rc_invites:
  per_room:
    per_second: 10000
    burst_count: 10000
  per_user:
    per_second: 10000
    burst_count: 10000
rc_room_creation:
  per_second: 10000
  burst_count: 10000
```


**⚠️ Önemli:** Synapse'i yeniden başlatmayı (`systemctl restart matrix-synapse`) ve güvenlik için migrasyon tamamlandıktan sonra **hız sınırlamayı tekrar etkinleştirmeyi** unutmayın!

### ⚠️ Hız Sınırlaması Hakkında Önemli Uyarı

Tüm hız sınırı bypass ayarlarını yapılandırsanız bile, hız sınırlaması veya geçici ağ sorunları nedeniyle **bazı öğeler aktarılamayabilir**. **Panik yapmayın!**

Migrasyon aracı **devam ettirilebilir** olarak tasarlanmıştır:
- Zaten aktarılmış kullanıcılar, space'ler ve odalar mapping dosyasında takip edilir
- Import komutunu tekrar çalıştırdığınızda, **zaten aktarılmış öğeleri atlayacak** ve sadece başarısız olanları işleyecektir
- Birkaç dakika bekleyin ve aynı import komutunu tekrar çalıştırın
- Tüm öğeler başarıyla aktarılana kadar tekrarlayın

Bu normal bir davranıştır ve araç sonunda tüm aktarımları tamamlayacaktır.

## Application Service Kurulumu (Mesaj Aktarımı için)

Mesajları **orijinal zaman damgalarıyla** aktarmak için Synapse sunucunuzda bir Application Service (AS) yapılandırmanız gerekir. AS olmadan mesajlar mevcut zaman damgasıyla aktarılır.

**Not:** Bağlantı testi, Application Service yapılandırılmamışsa bir uyarı (⚠) gösterecek ve mesaj zaman damgalarının korunmayacağını hatırlatacaktır.

### Adım 1: Token'ları Oluşturun

```bash
# AS token oluştur
openssl rand -hex 32
# Örnek çıktı: a1b2c3d4e5f6...

# HS token oluştur
openssl rand -hex 32
# Örnek çıktı: 9z8y7x6w5v4u...
```

### Adım 2: Registration Dosyası Oluşturun

Synapse sunucunuzda bir dosya oluşturun (örn. `/etc/matrix-synapse/matrixmigrate.yaml`):

```yaml
id: matrixmigrate
url: null  # Callback URL gerekli değil - sadece giden
as_token: "OLUŞTURDUĞUNUZ_AS_TOKEN"
hs_token: "OLUŞTURDUĞUNUZ_HS_TOKEN"
sender_localpart: matrixmigrate
rate_limited: false  # AS için hız sınırlamasını devre dışı bırak
namespaces:
  users: []
  rooms: []
  aliases: []
```

### Adım 3: Synapse'e Kaydedin

`homeserver.yaml` dosyanıza ekleyin:

```yaml
app_service_config_files:
  - /etc/matrix-synapse/matrixmigrate.yaml
```

Ardından Synapse'i yeniden başlatın:

```bash
systemctl restart matrix-synapse
```

### Adım 4: MatrixMigrate'i Yapılandırın

`config.yaml` dosyanıza ekleyin:

```yaml
matrix:
  appservice:
    enabled: true
    as_token_env: "MATRIX_AS_TOKEN"
```

Ortam değişkenini ayarlayın:

```bash
export MATRIX_AS_TOKEN="OLUŞTURDUĞUNUZ_AS_TOKEN"
```

### Adım 5: Mesajları Aktarın

```bash
./matrixmigrate import messages
```

**Not:** AS token'ı, migrasyon aracının kullanıcılar adına orijinal zaman damgalarıyla mesaj göndermesini sağlar. Mesaj geçmişini doğru şekilde korumak için tek yol budur.

---

## Sorun Giderme

Bağlantının tam olarak nerede başarısız olduğunu belirlemek için `./matrixmigrate test all` kullanın.

### SSH Bağlantısı Başarısız
- Anahtar doğrulama için: SSH anahtarının düzgün yapılandırıldığından ve doğru izinlere sahip olduğundan emin olun
- Şifre doğrulama için: Şifre ortam değişkeninin ayarlandığını kontrol edin
- SSH portunun doğru olduğunu doğrulayın (varsayılan: 22)

### Mattermost Config Bulunamadı
- config.yaml dosyanızdaki `config_path` değerini kontrol edin
- Farklı yolları deneyin: `/opt/mattermost/config/config.json`, `/opt/mattermost/config.json`
- SSH kullanıcısının dosyaya okuma erişimi olduğundan emin olun

### Matrix Girişi Başarısız
- Admin kullanıcı adı ve şifresini doğrulayın
- Matrix homeserver'ın şifre girişini destekleyip desteklemediğini kontrol edin
- Kullanıcının admin yetkilerine sahip olduğundan emin olun

### Veritabanı Bağlantısı Başarısız
- Araç, kimlik bilgilerini Mattermost'un config.json dosyasından otomatik olarak okur
- PostgreSQL'in çalıştığından ve Mattermost sunucusunda localhost'tan erişilebilir olduğundan emin olun

### Application Service Uyarısı
- Bağlantı testinde "⚠ Application Service (Yapılandırılmamış)" görüyorsanız, bu şu anlama gelir:
  - Mesajlar orijinal zaman damgaları yerine mevcut zaman damgasıyla aktarılacak
  - Düzeltmek için: Yukarıdaki "Application Service Kurulumu" bölümünü takip edin

## Lisans

MIT Lisansı
