# pbctl

A command line client for [PocketBase](https://pocketbase.io), built for AI agents and still pleasant for people.

- **Every instance, one tool.** Named profiles for local, staging and production. Works with any PocketBase 0.23 or newer over its REST API; nothing to install on the server.
- **Small output.** Tab-separated tables, 20 rows a page, long values cut and marked, a one-line schema view, one-line log entries. `-o json` gives the exact API response when you need it.
- **Read-only that holds.** A profile is read-only unless a person says otherwise. For production there are two modes the agent cannot switch off: a gateway that keeps the credential, and a server-side guard that makes a superuser read-only inside PocketBase.
- **The whole API.** Records, collections, files, logs, settings, backups, crons, SQL, auth flows, realtime, batch, and `pbctl api` for anything else, including your own routes.

```
$ pbctl collections show posts
posts (base) id=pbc_1125843985
fields:
  id text required system primaryKey autogeneratePattern=[a-z0-9]{15} max=15 min=15 pattern=^[a-z0-9]+$
  title text required max=200
  author relation ->users maxSelect=1
  tags select maxSelect=2 values=["a","b"]
rules:
  list: @request.auth.id != ""
  view: (anyone)
  create: (superusers only)
indexes:
  CREATE INDEX idx_title ON posts (title)

$ pbctl logs list --level error --since 1h -n 2
id	created	level	message	details
jrsjaf78mq34jgw	2026-10-06 19:42:11	ERROR	POST /api/collections/posts/records	status=400 execTime=0.2ms auth=_superusers error=Failed to create record.
7pqr79kwg3ta83n	2026-10-06 19:42:11	ERROR	GET /api/collections/nope/records?page=1&perPage=20	status=404 execTime=0.1ms error=Missing collection context.

$ pbctl -p prod records create posts title=x
error: read-only: POST /api/collections/posts/records was not sent (read-only is set by the profile)
hint: nothing was changed; writes need a profile that is not read-only, and only a person can set one up
```

## Install

```bash
go install github.com/Barney241/pocketbase-cli/cmd/pbctl@latest
```

Or build from a checkout with `make build`.

## Quick start

```bash
# a local instance you may write to (asks for the password and for a typed confirmation)
pbctl profile add local --url http://127.0.0.1:8090 --identity admin@example.com --writable

# production, read-only, password taken from the environment at run time
pbctl profile add prod --url https://pb.example.com --identity agent@example.com --password-env PB_PROD_PASSWORD

pbctl status                      # which instance, who you are, what enforces read-only
pbctl collections list
pbctl -p prod records list posts -f "status = 'open'" -s -created --fields id,title
pbctl guide                       # the one-page reference, written for an agent's context window
```

Without a profile, `PBCTL_URL` with `PBCTL_TOKEN` or `PBCTL_IDENTITY` + `PBCTL_PASSWORD` is enough.

### Telling an agent about it

Put this in `CLAUDE.md`, `AGENTS.md` or your system prompt:

```
Use `pbctl` for anything PocketBase: data, schema, logs, files. Run `pbctl guide` once for the syntax.
Profiles: `local` (writable), `prod` (read-only). Never work around a read-only refusal; ask me instead.
```

## Commands

| Area | Read | Write |
|---|---|---|
| `records` | `list` `get` `count` `watch` | `create` `update` `delete` `import` `batch` |
| `collections` | `list` `show` `export` `diff` `scaffolds` `dry-run-view` | `create` `update` `delete` `truncate` `import` |
| `files` | `list` `url` `download` | upload with `records create/update --file field=path` |
| `logs` | `list` `get` `stats` | `truncate` |
| `settings` | `get` | `update` `test-s3` `test-email` `apple-client-secret` |
| `backups` | `list` `download` | `create` `upload` `delete` `restore` |
| `crons` | `list` | `run` |
| `sql` | | `sql "<query>"` (the endpoint can write, so it counts as a write) |
| `auth` | `methods` `login` `refresh` | `request-otp` `with-otp` `request-verification` `confirm-verification` `request-password-reset` `confirm-password-reset` `request-email-change` `confirm-email-change` `impersonate` |
| `api` | `api GET <path>` | `api POST|PATCH|PUT|DELETE <path>` |
| setup | `status` `guide` `profile list` | `profile add|use|remove|lock|unlock` `gateway` `guard hook` |

Things worth knowing:

- **`--as <collection>/<id>`** runs any command as that user, so you can see exactly what their API rules let through. The token is never printed.
- **`--param`** quotes filter values for you: `-f 'title = {:t}' --param t="O'Brien"`.
- **`key=value` and `key:=json`** build request bodies without JSON quoting: `pbctl records create posts title=Hi views:=0`.
- **`collections diff <other-profile>`** compares two instances' schemas by name: the fastest way to see what a deploy will change.
- **`collections dry-run-view "SELECT ..."`** is a read-only SQL sample (10 rows) that works on every version.
- **`--dry-run`** prints the write request and sends nothing. Destructive commands need `--yes`; a profile with `confirm_writes` needs it for every write.
- **`records watch`** stops by itself after a minute when no terminal is attached, so an agent run cannot hang.

Exit codes: `0` ok, `1` request failed, `2` bad usage, `3` blocked by read-only, `4` not authorised, `5` not found, `6` needs `--yes` or a person.

## Read-only

There are three levels. Pick by how much you trust what holds the credential.

| Level | What stops a write | Holds against | Setup |
|---|---|---|---|
| 1. Client | pbctl refuses to send it | mistakes, and an agent that only uses pbctl | default for every new profile |
| 2. Gateway | a proxy that forwards reads only; the agent never has the credential | an agent with a full shell | `pbctl gateway`, run by you |
| 3. Server guard | PocketBase refuses writes from that superuser | anyone holding that superuser's password or token | a `pb_hooks` file or a Go import |

`pbctl status` tells you which one is in force:

```
mode: read-only, set by the profile
enforcement: this client only (pbctl sends no writes; the credential itself could still write if used elsewhere)
```

### Level 1: the client

Read-only is on when **any** of these says so, and nothing can turn it off for a single command:

- the profile (`"read_only": true`, the default from `profile add`),
- `PBCTL_READ_ONLY=1`,
- `--read-only`,
- the system policy file `/etc/pbctl/policy.json`, which a non-root user cannot edit:

  ```json
  { "read_only_hosts": ["pb.example.com", "*.prod.example.com"] }
  ```

  (`{"read_only": true}` covers every host.)

In read-only mode one HTTP transport, which every request passes through, lets out `GET`, `HEAD`, `OPTIONS` and five `POST` routes that change nothing: password login, token refresh, file token, realtime subscription and view dry-run. Everything else is refused before a connection is opened, with exit code 3.

`profile unlock` and `profile add --writable` ask for a typed confirmation on an interactive terminal and refuse without one.

Be clear about the limit: at this level the agent's user can read the stored credential and the config file. It stops accidents and it stops pbctl. It does not stop `curl`. For that, use level 2 or 3.

### Level 2: the gateway

```bash
# in your own terminal; the password is asked for and kept in memory only
pbctl gateway --upstream https://pb.example.com --identity admin@example.com

# what the agent gets: a URL and nothing else
pbctl profile add prod --url http://127.0.0.1:8099
```

The gateway logs in upstream itself and forwards `GET`/`HEAD` under `/api/` plus plain realtime subscriptions. It never returns an auth token or a file token: protected files are fetched with a token the gateway adds on the way out, and `--as` is done inside the gateway. It also refuses the read requests that would let a reader become a writer:

- backup downloads (a backup contains the secrets that sign auth tokens),
- the SQL endpoint and view dry-runs (they can read those same secrets from the database),
- filters and sorts that mention `password` or `tokenKey` (superusers may filter on hidden fields, which leaks them one character at a time),
- realtime subscriptions that carry options (they can hold a filter),
- tokens inside log entries: PocketBase logs request URLs, and a file token in a `?token=` parameter is enough to download a backup, so the gateway blanks every token it finds in `/api/logs` responses.

Against other things on your machine: it listens on loopback, checks the `Host` header, refuses any request sent by a web page, strips CORS headers from responses, and can require a key (`PBCTL_GATEWAY_KEY`, mandatory when listening on a non-loopback address). On Linux the process marks itself non-dumpable so another process of the same user cannot read the password out of its memory. Every request is logged to stderr, so you can watch what the agent reads.

Keep the upstream password out of anything the agent can read (`.env` files, shell history, the pbctl config). If the agent runs as your user and the password sits in a file, the gateway cannot help. Running the gateway as another user, in a container or on another host removes that concern.

### Level 3: the server guard

Make one superuser read-only inside PocketBase. Whatever client holds its password, writes fail.

Stock PocketBase:

```bash
pbctl guard hook --superuser agent@example.com > pb_hooks/pbctl_readonly.pb.js
```

PocketBase used as a Go framework:

```go
import "github.com/Barney241/pocketbase-cli/pbguard"

app.OnServe().BindFunc(func(se *core.ServeEvent) error {
    pbguard.Bind(se, "agent@example.com")
    return se.Next()
})
```

The guarded superuser can read records, schema, logs, settings and files, and refresh its own token. It cannot write, impersonate, run SQL or view dry-runs, download backups, or filter by `password`/`tokenKey`.

The guard also keeps file tokens out of the request log (`?token=REDACTED`) for every user, because a logged file token of a full superuser would let a log reader download a backup. Log entries written before the guard was installed still hold tokens; they expire three minutes after they were issued.

### What no level can know

- A custom `GET` route in your app that changes data looks like a read. Don't write those, or keep them away from the account you hand out.
- Read access is still access: a read-only superuser sees every record. Read-only protects integrity, not confidentiality.
- Logging in can itself have effects on the server: PocketBase may send a "new login" alert email. pbctl caches the token (in your user cache directory, mode 0600) to log in as rarely as possible.

## Output

| Format | Use |
|---|---|
| `table` (default) | tab-separated with a header when piped, aligned on a terminal; cells over 120 characters are cut and marked `…(+N)` |
| `-o json` | the exact API response, never cut |
| `-o jsonl` | one object per line, never cut |

Notes such as `rows 21-40 of 231; next: --page 3` go to stderr; `-q` silences them. `--fields` is passed to the server and is the best way to keep a response small. Downloads are written to disk and only the path, size and type are printed.

## Configuration

Profiles live in `~/.config/pbctl/config.json` (mode 0600, override with `PBCTL_CONFIG`):

```json
{
  "current": "local",
  "profiles": {
    "prod": {
      "url": "https://pb.example.com",
      "identity": "agent@example.com",
      "password_env": "PB_PROD_PASSWORD",
      "read_only": true,
      "confirm_writes": true
    }
  }
}
```

| Key | Meaning |
|---|---|
| `url` | instance or gateway URL; a path prefix is fine |
| `auth_collection` | defaults to `_superusers`; any auth collection works |
| `identity`, `password` / `password_env` | login; the `_env` form names a variable read at run time |
| `token` / `token_env` | use a token instead of logging in |
| `gateway_key` | sent to a pbctl gateway that requires one |
| `read_only` | level 1 read-only |
| `confirm_writes` | require `--yes` for every write |

Environment: `PBCTL_PROFILE`, `PBCTL_OUTPUT`, `PBCTL_READ_ONLY`, `PBCTL_CONFIG`, and the profile-less `PBCTL_URL`, `PBCTL_TOKEN`, `PBCTL_IDENTITY`, `PBCTL_PASSWORD`.

## Development

```bash
make test          # unit tests
make integration   # builds a PocketBase test server and runs the suite against it
```

The integration suite runs every write command against a real PocketBase under each read-only level and checks that nothing changed. `pbguard` is its own Go module so that pbctl does not depend on PocketBase; tag its releases as `pbguard/vX.Y.Z`.

Not included: generating or running migration files. PocketBase's own `migrate collections` does that from inside the server, where it belongs; `collections export`, `import` and `diff` cover the over-the-API side.

## Credits

The command surface follows the tool sets of [mabeldata/pocketbase-mcp](https://github.com/mabeldata/pocketbase-mcp) and [mrwyndham/pocketbase-mcp](https://github.com/mrwyndham/pocketbase-mcp).

## License

MIT
