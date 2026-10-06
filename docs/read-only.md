# Read-only levels

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
