# Code Review Log

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

## 2026-03-24 — Stuck session fix

**Issues found (HIGH):**

1. **Bug: `processing` flag not cleared on `process_exited` or `error`** — If the Claude process crashes mid-response or LM Studio emits a streaming error, `session.processing` remains `true`. Since `SendMessage` rejects new messages when `processing == true`, the session is permanently stuck — the user cannot send another message without clearing or deleting the session. Only `message_complete` cleared the flag; the two failure-path events did not.

**Fixes applied:**

- `session.go`: `handleProviderEvent` now clears `session.processing` on both `process_exited` and `error` events (matching existing `message_complete` behavior). Also calls `saveSession` on `process_exited` to persist the cleared state.
