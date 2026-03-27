# Code Review Log

## 2026-03-26 — Full codebase review (clean)

**No HIGH priority issues found.** Reviewed all 18 Go source files. Verified uncommitted changes (6 bug fixes across `api.go`, `session.go`, `provider_claude.go`, `provider_lmstudio.go`) are correct. Traced event flow, lock ordering, double-emit edge cases, and collector lifecycle. Cross-referenced all prior review entries — no regressions or flip-flops. Build and vet pass.

## 2026-03-26 — LM Studio stuck session on clean stream end

**Issues found (HIGH):**

1. **Bug: `streamResponse` exits without terminal event on clean EOF** — If the LM Studio SSE stream ends cleanly (EOF or `[DONE]`) without a `chat.end` event — due to server-side issues, protocol mismatch, or abnormal termination — `streamResponse` returned without emitting `message_complete` or `error`. The session's `processing` flag stays `true` permanently and no further messages can be sent. The Claude provider is immune because `waitForExit()` always fires `process_exited`, but LM Studio had no equivalent fallback.

**Fixes applied:**

- `provider_lmstudio.go`: Track whether a terminal event (`chat.end` or `error`) was processed during the stream. After the scanner loop, if the stream ended without error and no terminal event was seen, emit `message_complete` with accumulated text as a fallback. Added early `return` after the scanner error path to prevent double-emit.

## 2026-03-26 — Broken session resume for HTTP clients

**Issues found (HIGH):**

1. **Bug: `DELETE /api/sessions/:id` permanently deletes instead of ending the session** — Both `DELETE /api/sessions/:id` and `POST /api/sessions/:id/delete` called `DeleteSession` (removes from memory + deletes from disk). CLAUDE.md documents `DELETE` as "end session", which should preserve to disk for future resume. Only the WS `end_session` handler correctly called `EndSession`. HTTP clients (relayTelegram, relayScheduler) could never gracefully end sessions — only permanently delete them.
2. **Bug: `SendMessage`/`SendMessageSync` don't lazy-load sessions from disk** — After `EndSession` removes a session from the in-memory map (or after a server restart), `SendMessage` and `SendMessageSync` used direct map lookups and returned "session not found" instead of lazy-loading from disk like `GetSession` does. This made session resume impossible for HTTP clients.

**Fixes applied:**

- `api.go`: `DELETE /api/sessions/:id` now calls `EndSession` (persist to disk) instead of `DeleteSession` (permanent removal). `POST /api/sessions/:id/delete` remains as the permanent delete route.
- `session.go`: `SendMessage` now uses `GetSession` (which lazy-loads from disk) instead of direct map lookup.
- `session.go`: `SendMessageSync` now uses `GetSession` to ensure the session is loaded before registering the collector.

## 2026-03-24 — Data race fixes and dead code cleanup

**Issues found (HIGH):**

1. **Data race: `ClaudeProvider.claudeSessionID`** — Written by `processLine` (stdout goroutine), read by `GetState`, `DeleteSession`, `RestoreState` without synchronization.
2. **Data race: `LMStudioProvider.responseID`** — Written by `streamResponse` goroutine in `handleSSEEvent`, read by `GetState`/`SendMessage` without consistent locking.
3. **Data race: `LMStudioProvider.cancelFn`** — Set under `p.mu` in `doSend`, read in `StopGeneration` without lock. Realistic scenario: user clicks stop while streaming.
4. **Race window: `ClaudeProvider.Kill()` vs `SendMessage`** — `Kill()` closed stdin before setting `alive = false`. Concurrent `SendMessage` could pass the alive check then write to closed stdin.
5. **Dead code: `_ = sc` in `api.go`** — Unnecessary suppression; variadic parameter replaced with plain `*SchedulerClient`.

**Fixes applied:**

- `provider_claude.go`: Protected `claudeSessionID` with `p.mu` in `processLine`, `GetState`, `RestoreState`, `DeleteSession`. Moved `alive.Store(false)` to top of `Kill()`.
- `provider_lmstudio.go`: Protected `responseID` with `p.mu` in `handleSSEEvent` (chat.end), `GetState`, `RestoreState`. Protected `cancelFn` with `p.mu` in `StopGeneration`.
- `api.go`: Changed `RegisterProjectRoutes` from variadic to plain `*SchedulerClient` param, removed `_ = sc`.
- `integration_test.go`: Pass `nil` for scheduler client.
- Removed stray double blank lines in `main.go`, `session.go`, `provider_claude.go`.

## 2026-03-24 — Panic fix, provider race, double Wait

**Issues found (HIGH):**

1. **Panic: `ResponseCollector` double-close of `c.done`** — `message_complete` and `error` cases both called `close(c.done)` unconditionally. If LM Studio emits `chat.end` (message_complete) then scanner.Err fires (error), the channel is double-closed causing a panic. The `process_exited` case had a select guard but the other two did not.
2. **Race: `session.provider` accessed without synchronization** — `ClearSession` sets `session.provider = nil` without holding `session.mu`, while `SendMessage`, `StopGeneration`, `ListSessions`, etc. read it concurrently. A clear during an in-flight send causes a nil pointer dereference.
3. **Data race: double `cmd.Wait()` in `ClaudeProvider`** — `Kill()` spawned a goroutine calling `cmd.Wait()` while the `waitForExit()` goroutine also calls `cmd.Wait()`. The `finished` field in `exec.Cmd` is not goroutine-safe; this is a data race.

**Fixes applied:**

- `response_collector.go`: Added `doneOnce sync.Once` field. All three close sites now use `c.doneOnce.Do(func() { close(c.done) })`.
- `session.go`: Added `getProvider()`/`setProvider()` helpers on `Session` that access `session.provider` under `session.mu`. Updated `SendMessage`, `StopGeneration`, `EndSession`, `DeleteSession`, `ClearSession`, `StopAll`, `ListSessions`, and `initProvider` to use them.
- `provider_claude.go`: Added `waitDone chan struct{}` closed by `waitForExit()`. `Kill()` now waits on `waitDone` instead of calling `cmd.Wait()` directly, eliminating the double-Wait race.

## 2026-03-26 — Stuck session on send failure, hook config clobber, event encoding

**Issues found (HIGH):**

1. **Bug: `processing` flag stuck when `provider.SendMessage()` returns error** — If the provider rejects the message (dead process, stdin error, HTTP failure), `processing` stays `true` and no provider event fires to clear it. The session is permanently stuck. Different from the prior "process_exited/error event" fix — this is the `SendMessage` return-value path.
2. **Bug: `ensureHookConfig` clobbers all existing hooks** — The entire `hooks` key was overwritten, destroying any pre-existing hook types (PostToolUse, etc.) in the project's `settings.local.json`.
3. **Bug: `raw_output` event double-encoded** — Non-JSON stdout was JSON-marshaled into `{"text":"..."}`, then `handleProviderEvent` used `string(data)` producing JSON-inside-a-string for Eve.
4. **Bug: LM Studio scanner error double-quoted** — `json.Marshal(err.Error())` wrapped the error in JSON quotes, then `string(data)` preserved the literal quote chars in the WS error message.

**Fixes applied:**

- `session.go`: `SendMessage` now clears `session.processing` when `provider.SendMessage()` returns an error.
- `session.go`: `ensureHookConfig` merges into existing `hooks` map instead of replacing it, preserving other hook types.
- `provider_claude.go`: `processLine` passes raw bytes directly to handler for `raw_output` instead of double-encoding via `json.Marshal`.
- `provider_lmstudio.go`: `streamResponse` passes error string directly as `json.RawMessage` instead of JSON-encoding it.

## 2026-03-24 — Stuck session fix

**Issues found (HIGH):**

1. **Bug: `processing` flag not cleared on `process_exited` or `error`** — If the Claude process crashes mid-response or LM Studio emits a streaming error, `session.processing` remains `true`. Since `SendMessage` rejects new messages when `processing == true`, the session is permanently stuck — the user cannot send another message without clearing or deleting the session. Only `message_complete` cleared the flag; the two failure-path events did not.

**Fixes applied:**

- `session.go`: `handleProviderEvent` now clears `session.processing` on both `process_exited` and `error` events (matching existing `message_complete` behavior). Also calls `saveSession` on `process_exited` to persist the cleared state.
