# Reference

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
| setup | `status` `guide` `profile list` `mcp` | `profile add|use|remove|lock|unlock` `gateway` `guard hook` |

Things worth knowing:

- **`--as <collection>/<id>`** runs any command as that user, so you can see exactly what their API rules let through. The token is never printed.
- **`--param`** quotes filter values for you: `-f 'title = {:t}' --param t="O'Brien"`.
- **`key=value` and `key:=json`** build request bodies without JSON quoting: `pbctl records create posts title=Hi views:=0`.
- **`collections diff <other-profile>`** compares two instances' schemas by name: the fastest way to see what a deploy will change.
- **`collections dry-run-view "SELECT ..."`** is a read-only SQL sample (10 rows) that works on every version.
- **`--dry-run`** prints the write request and sends nothing. Destructive commands need `--yes`; a profile with `confirm_writes` needs it for every write.
- **`records watch`** stops by itself after a minute when no terminal is attached, so an agent run cannot hang.

Exit codes: `0` ok, `1` request failed, `2` bad usage, `3` blocked by read-only, `4` not authorised, `5` not found, `6` needs `--yes` or a person.
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
| `identity` / `identity_env`, `password` / `password_env` | login; the `_env` form names a variable read at run time |
| `token` / `token_env` | use a token instead of logging in |
| `gateway_key` | sent to a pbctl gateway that requires one |
| `header_env` | extra request headers, `{"X-Header": "VAR"}`, each read from the named variable at run time; for a proxy in front of the instance that wants its own token. `Authorization` and the `X-Pbctl-*` headers cannot be set |
| `read_only` | level 1 read-only |
| `confirm_writes` | require `--yes` for every write |

Environment: `PBCTL_PROFILE`, `PBCTL_OUTPUT`, `PBCTL_READ_ONLY`, `PBCTL_CONFIG`, and the profile-less `PBCTL_URL`, `PBCTL_TOKEN`, `PBCTL_IDENTITY`, `PBCTL_PASSWORD`.

## Not included

Generating or running migration files. PocketBase's own `migrate collections` does that from inside the server; `collections export`, `import` and `diff` cover the over-the-API side.
