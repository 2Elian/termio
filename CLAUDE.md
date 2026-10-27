# Termio repository instructions

## Git and remote

- **Standing authorization**: when the user asks to push, says "推送到远程", "提交并推送", or otherwise expresses intent to publish changes in this repository, commit and push to `origin main` without asking for confirmation first.
- When a code change is complete and `go test -count=1 ./...` and `go vet ./...` pass, you may commit and push automatically if the change is clearly part of the requested work.
- Never force-push, rewrite remote history, or push to a different remote/branch without explicit approval.
- Before pushing code changes, run `gofmt -l .` (must be empty), `go vet ./...`, and `go test -count=1 ./...`.
- Do not commit secrets or local state: `.debug/`, `devices.json`, `devices.local.json`, `.env`, local token/config files. They are in `.gitignore`.
- `a.py` in the repository root is unrelated to Termio; leave it untracked unless the user explicitly asks.
- End commit messages with:

```text
Co-Authored-By: Claude Code <noreply@anthropic.com>
```

## Project conventions

- Docs live in `docs/`; update `README.md` and the relevant docs when behavior changes.
- `$LARES` is the per-user app home, default `~/.lares`; terminal metadata (config, token, logs) lives in `$LARES/.termio`.
- The default workspace is `$LARES`. Tools must never be able to read `$LARES/.termio`; the sandbox deny list protects it.
- The reference gateway is single-instance and not production multi-tenant. Do not describe it as HA or production-ready.
- Keep the daemon wire protocol stable; add extensions under `_meta` or in local config rather than breaking JSON-RPC shapes.
