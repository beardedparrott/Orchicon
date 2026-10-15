# Release Highlights

> Curated, reader-facing highlights for the current release cycle.
>
> This file is the narrative preface for two surfaces:
>   - the GitHub Release body (prepended above the detailed UPDATES.md
>     rows by `scripts/gen-release-notes.sh`), and
>   - README.md's "Last Release Changes" section (the first paragraph
>     only, synced by the same script — keep it tight; the full story
>     lives on the GitHub release page).
>
> Update this file at the START of a release cut (before the develop→main
> merge PR): describe what shipped as whole features — what it is, why
> you'd use it — not commit-by-commit detail. The `## vX.Y.Z` heading
> tells the tooling which version these highlights describe; update it
> when you cut.

## v0.5.43

### New: an installer you can trust with more than one stack
The installer grew a real vocabulary and a measure of consent. `--instance dev|prod|test` scopes **every**
action to one stack, so installing, updating or removing one can no longer reach into another — and
`--uninstall`, `--purge` and `--remove-binary` are now separate verbs, because "Uninstall" that leaves a web
server running (or takes your data with it) is not one operation. Installing over an existing instance
**converges** it rather than doing something adjacent: a present image tag is no longer mistaken for a current
one, and the canary ships an image so a canary install serves a UI at all. Windows reached parity with the
POSIX path — the uninstall verbs, the instance prompt, and the URLs, which had been **guessing at ports**.
Every destructive verb is instance-bound, including `-ForceClean`, which had been acting without asking which
stack you meant.

### New: a canary install path on `develop`
The installer could only be tested after it shipped, which is how two releases carried a broken one. There is
now a canary install path on `develop`, plus a real install-path test that runs the **actual** installer
against a locally-served artifact and a stub docker, so it needs no network, no Docker and no release. It is
a standing gate on the POSIX path, and the Windows path has a harness that cannot touch a real resource.

### New: one profile per instance, and one instance per process
`orch` is meant to run more than once at a time; it could not. Every profile was named the literal `default`,
so connecting to a second instance **overwrote the first one's** URL and tokens in place — that was the whole
of "it seems to flop which port you are connecting to each time". Profiles are now keyed by the instance's
host:port, and `ORCHICON_PROFILE` (or `--profile`) selects one **for this process** without touching the
global active profile, so one shell can be dev while another is prod. An unknown name resolves to no profile
and says so, rather than silently reading the other instance's conversations.

Alongside it, a config write can no longer destroy a **different** instance's profile: `Save` was truncating
in place, so a reader during a write could see a half-written document — and the recovery path for an
unreadable config is to start empty and then **save**, which would have overwritten it. Writes are now atomic
(temp file, fsync, same-directory rename), and an unreadable config is **moved aside** with its reason shown
instead of being discarded in silence.

### New: in-transcript search, in both clients
Find a phrase in a long conversation and step through the matches with `n`/`N` (TUI) or prev/next (GUI),
with jump-to-top and jump-to-bottom for navigating a conversation you are already in. Every occurrence is now
**highlighted in the text**, so a match in a long reply is something you can see rather than something you
have to hunt for — the filter answered "which message", and this answers "which line".

### New: the Ask pane is live as it is written
A reply now streams into the transcript as the model writes it, instead of appearing only when the turn
finished. The card asking you a clarifying question, or for a permission, is drawn **after** the text it is
about — information, then the question — and out of the way of the running reply. A card raised while you are
looking at another window now reaches you without a reload, and answering one in either client settles it in
both.

### Also in this release
- **A payload-too-large refusal recovers instead of wedging.** A `413` from the gateway matched none of the
  known limits, so the oversize-prompt hatch was never reached and every retry re-sent the identical body.
  A transport body cap and a model context window are different limits with the same remedy — send fewer
  bytes — and both now take it.
- **A restart can no longer lose your conversation.** The history reducer knew about images and nothing else,
  so a turn whose bulk was base64 **tool results** reclaimed nothing and wrote no file at all — leaving a
  stale snapshot that only the restart path consults.
- **Freeing `ctrl+f` for search.** It had been "attach a file by path" in the TUI and nothing in the GUI;
  `/attach` now does the attaching, and the chord does the finding.
- **A permission card settles on the server's answer**, not on a guess about whether a turn is still running.
- **The release pruner had been a silent no-op its whole life** — the cap it exists to enforce was never
  enforced.
- **The GUI's default theme is Teal Depths again**, three points lighter, and the app background is a solid
  colour — the ambient swirls were legible only in the dark themes.
- **A canary binary no longer ships without a frontend**, which had made a canary install serve an auth error
  instead of a UI.
- **The footer band stops jumping.** An empty card line was collapsing it, and the screen bounced.
- **The installer's common commands are in the install docs**, rather than only in the README's flags.

## v0.5.39

### New: OpenCode 2.x
Orchicon runs on the current opencode, and on the previous one. The version is chosen automatically from
the serve's own reported version, so an install you already have keeps working and a fresh one is not held
back. The two are **different wire protocols**, not two settings on one, and what that cost was almost
entirely invisible: a run against 2.x could **not finish at all**, because the vocabulary its event stream
uses is not the one the platform was reading, so the end of a turn looked exactly like the middle of one.
Nothing errored — the turn simply never completed. Alongside it, the telemetry classifiers were reading 1.x
part names, which meant a **working** 2.x run could be killed as a stalled one: real work, stopped for the
wrong reason. Both are fixed, unknown event shapes are passed through rather than dropped, and a worker's
model reference now reaches the runtime in the form it expects.

### New: a runtime container that can start an adapter
`serve handshake: exit status 1`, on every workflow execution using the opencode adapter. The runtime was
never the problem: Docker creates a bind mount's missing parents **as root**, `$HOME` among them, so the
container's own home directory was root-owned and the adapter could not create anything under it. That broke
**both** opencode versions, with a different error each (`EACCES` on `.cache` for 1.x, an unwritable data
home for 2.x), and neither appeared in the daemon's own logs. The mount shape is fixed, and the failure that
had been reporting itself as a warning nobody read is now impossible.

### New: install and WSL, finished
The adapter CLI has to be installed as your own user, and inside WSL on Windows. That is not a preference:
a standalone dispatch runs on the plane and sees your `PATH`, while a workflow run executes in its own
container that sees only the adapter paths mounted under your home directory. A system-wide `sudo npm i -g`
is invisible to every workflow run, and a Windows-side install is reachable through WSL interop — which is
what makes it *look* installed while nothing can use it. The guide now states the rule, the reason, and a
per-install-method table of what each one can actually reach.

Related: "where opencode lives" was resolved in six places and they disagreed. Only model *discovery*
honoured `ORCHICON_OPENCODE_BIN`, the very variable the plane's error message tells you to set — so an
operator who **followed the instruction** fixed discovery and left execution pointing at a different binary
or none, with the failure surfacing somewhere else entirely. There is now one resolver, and an override that
names nothing is refused rather than silently ignored.

### New: `orchicon restart`
There is now a command that restarts a plane, and it says so. A plane captures its environment **when it
starts**, so installing an adapter CLI afterwards changed nothing until it restarted — and `orchicon install`
deliberately does not restart a plane that is already healthy on the installed binary. The only way to
refresh it was `serve --stop` followed by an install: a restart in disguise, nested under a command about
serving, named as neither, and absent from `--help` altogether.

### New: WSL renders the themes' real colour
Every dark theme showed **black** instead of its tint, and the palette was never the problem. The 256-colour
cube's first non-zero level is 95/255, so its **only** dark entries are greys — every dark ground in every
theme quantises to black, and no amount of lightening fixes it. Windows Terminal does 24-bit colour but does
not reliably **advertise** it into a WSL shell, and rendering follows the advertisement. Orchicon now
supplies it, in-process, where it can reach the TUI. `COLORTERM=yes` still pins 256-colour and
`ORCHICON_TRUECOLOR=0` turns it off.

### New: the execution diff shows your work
The diff panel for a workflow execution had recorded **nothing, ever**. Across two thousand executions, not
one row — for two independent reasons, both invisible. The reconciler read the **working tree** while the
worker is instructed to commit everything (so a tidy tree read as "no changes"), and each file's "before"
side was read from `HEAD` — which for a committed run *is* the change you are trying to see. Committed
changes are now reconciled from the run's fork point, and the panel stopped hiding the tail of a long line
behind an ellipsis.

### New: find, and navigate, from where you actually are
`ctrl+f` in the TUI opens a search box you type into, filtering the conversation as you go. The match count
is in front of you, `enter` keeps the search and hands the keyboard to the transcript, and `esc` puts back
the view you had. The old shape asked you to type the term in the message box and *then* press `ctrl+f`,
which did nothing at all from an empty composer — the state you are in while reading — and silently turned a
half-written message into a search term. The transcript's navigation keys (`home`, `end`, arrows,
`PgUp`/`PgDn`) now work from the composer too, which is where you are; before, they only worked after you
had clicked into the conversation, and an empty box swallowed them.

On a keyboard whose `home` and `end` are `fn`+arrow, those keys used to type the literal text **"OH"**. The
sequence such a keyboard sends is one the terminal library does not decode, so it arrived as two ordinary
characters. Fixed, and proven in a real terminal.

### Also in this release
- **The transcript stops yanking you back to the bottom.** Scrolling up, or jumping to the top, held: live replies no longer snap the view down.
- **Page 2 of every runs list works.** The pagination cursor sat in the wrong position, so a list's second page failed to parse while the first looked fine.
- **One log line per 200ms tick was 99.5% of all log volume.** It is a debug line now.
- **`orchicon serve` names the directory it cannot write**, instead of calling a fine policy file invalid.
- **The connection screen opens on username and password.** An API key cannot be had until you have logged in, so the TUI stopped asking for one.
- **Teal Depths is three points lighter** — a dark ground should read as tinted rather than black.
- **A rejected tool call keeps its reason.** The refusal was discarded before anyone read it, leaving a failed command with nothing to act on.
- **Rebuilt code can no longer be left serving.** A rebuild replaces the binary; a launcher that took "something answers on the port" as current left the old process running the superseded build.
- **A workspace's todo list survives the run**, and the transcript's changed-file list stops going blank mid-turn.


## v0.5.37

### New: OpenCode 2.x
Orchicon runs on the current opencode, and on the previous one. The version is chosen automatically from
the serve's own reported version, so an install you already have keeps working and a fresh one is not held
back. The two are **different wire protocols**, not two settings on one, and what that cost was almost
entirely invisible: a run against 2.x could **not finish at all**, because the vocabulary its event stream
uses is not the one the platform was reading, so the end of a turn looked exactly like the middle of one.
Nothing errored — the turn simply never completed. Alongside it, the telemetry classifiers were reading 1.x
part names, which meant a **working** 2.x run could be killed as a stalled one: real work, stopped for the
wrong reason. Both are fixed, unknown event shapes are passed through rather than dropped, and a worker's
model reference now reaches the runtime in the form it expects.

### New: a runtime container that can start an adapter
`serve handshake: exit status 1`, on every workflow execution using the opencode adapter. The runtime was
never the problem: Docker creates a bind mount's missing parents **as root**, `$HOME` among them, so the
container's own home directory was root-owned and the adapter could not create anything under it. That broke
**both** opencode versions, with a different error each (`EACCES` on `.cache` for 1.x, an unwritable data
home for 2.x), and neither appeared in the daemon's own logs. The mount shape is fixed, and the failure that
had been reporting itself as a warning nobody read is now impossible.

### New: install and WSL, finished
The adapter CLI has to be installed as your own user, and inside WSL on Windows. That is not a preference:
a standalone dispatch runs on the plane and sees your `PATH`, while a workflow run executes in its own
container that sees only the adapter paths mounted under your home directory. A system-wide `sudo npm i -g`
is invisible to every workflow run, and a Windows-side install is reachable through WSL interop — which is
what makes it *look* installed while nothing can use it. The guide now states the rule, the reason, and a
per-install-method table of what each one can actually reach.

Related: "where opencode lives" was resolved in six places and they disagreed. Only model *discovery*
honoured `ORCHICON_OPENCODE_BIN`, the very variable the plane's error message tells you to set — so an
operator who **followed the instruction** fixed discovery and left execution pointing at a different binary
or none, with the failure surfacing somewhere else entirely. There is now one resolver, and an override that
names nothing is refused rather than silently ignored.

### New: `orchicon restart`
There is now a command that restarts a plane, and it says so. A plane captures its environment **when it
starts**, so installing an adapter CLI afterwards changed nothing until it restarted — and `orchicon install`
deliberately does not restart a plane that is already healthy on the installed binary. The only way to
refresh it was `serve --stop` followed by an install: a restart in disguise, nested under a command about
serving, named as neither, and absent from `--help` altogether.

### New: WSL renders the themes' real colour
Every dark theme showed **black** instead of its tint, and the palette was never the problem. The 256-colour
cube's first non-zero level is 95/255, so its **only** dark entries are greys — every dark ground in every
theme quantises to black, and no amount of lightening fixes it. Windows Terminal does 24-bit colour but does
not reliably **advertise** it into a WSL shell, and rendering follows the advertisement. Orchicon now
supplies it, in-process, where it can reach the TUI. `COLORTERM=yes` still pins 256-colour and
`ORCHICON_TRUECOLOR=0` turns it off.

### New: the execution diff shows your work
The diff panel for a workflow execution had recorded **nothing, ever**. Across two thousand executions, not
one row — for two independent reasons, both invisible. The reconciler read the **working tree** while the
worker is instructed to commit everything (so a tidy tree read as "no changes"), and each file's "before"
side was read from `HEAD` — which for a committed run *is* the change you are trying to see. Committed
changes are now reconciled from the run's fork point, and the panel stopped hiding the tail of a long line
behind an ellipsis.

### New: find, and navigate, from where you actually are
`ctrl+f` in the TUI opens a search box you type into, filtering the conversation as you go. The match count
is in front of you, `enter` keeps the search and hands the keyboard to the transcript, and `esc` puts back
the view you had. The old shape asked you to type the term in the message box and *then* press `ctrl+f`,
which did nothing at all from an empty composer — the state you are in while reading — and silently turned a
half-written message into a search term. The transcript's navigation keys (`home`, `end`, arrows,
`PgUp`/`PgDn`) now work from the composer too, which is where you are; before, they only worked after you
had clicked into the conversation, and an empty box swallowed them.

On a keyboard whose `home` and `end` are `fn`+arrow, those keys used to type the literal text **"OH"**. The
sequence such a keyboard sends is one the terminal library does not decode, so it arrived as two ordinary
characters. Fixed, and proven in a real terminal.

### Also in this release
- **The transcript stops yanking you back to the bottom.** Scrolling up, or jumping to the top, held: live replies no longer snap the view down.
- **Page 2 of every runs list works.** The pagination cursor sat in the wrong position, so a list's second page failed to parse while the first looked fine.
- **One log line per 200ms tick was 99.5% of all log volume.** It is a debug line now.
- **`orchicon serve` names the directory it cannot write**, instead of calling a fine policy file invalid.
- **The connection screen opens on username and password.** An API key cannot be had until you have logged in, so the TUI stopped asking for one.
- **The default theme is Forest in both clients**, and Teal Depths is three points lighter — a dark ground should read as tinted, not black.
- **A rejected tool call keeps its reason.** The refusal was discarded before anyone read it, leaving a failed command with nothing to act on.
- **Rebuilt code can no longer be left serving.** A rebuild replaces the binary; a launcher that took "something answers on the port" as current left the old process running the superseded build.
- **A workspace's todo list survives the run**, and the transcript's changed-file list stops going blank mid-turn.

## v0.5.16

### New: a permission card you can finally answer
The card that asks whether a call may proceed had three ways of failing you, and all three are closed. A
card could vanish about two seconds after it appeared — drawn, instantly read as *settled*, dropped, then
re-drawn on a loop, so your click never landed. A card could be retired by a **connection event** rather
than by a decision: when the client's watch socket closed cleanly the turn looked finished, and every open
card was swept to "settled" while the plane still held the ask open. And a card could be pushed off the
**top** of the pane — the reply is anchored so it sinks below cards raised during it, which with a long
reasoning block walked the card out of view entirely, leaving just its hint row at the top. A card waiting
on you is now treated as a **prompt** rather than a transcript row: it stays pinned to the bottom of the
pane, and it is settled only by what the server records — your decision, an expiry, or a denial — never by
a guess about why a stream closed.

### New: the pane says what it is waiting for
The conversation's status line no longer reads the stream's silence while a card is waiting on you. It
used to say "no output for 40s — the stream will re-attach if it stays silent", which blamed the
connection and promised a re-attach while the stream was healthy and the turn was parked on **you**. It
now names the ask: `⏸ waiting for your approval · write src/main.go — Orchicon is paused until you answer`.

### New: a Windows installer that reports itself
On Windows the stack runs inside WSL2, and the installer had two ways of leaving you with nothing to go
on. It called `exit` — and under its own documented invocation (`irm … | iex`) that does not end the
script, it ends **your PowerShell session**, closing the window with the reason still unread. Its long
setup step also captured the output, so a working install was indistinguishable from a hung one; worse, a
captured *question* was a permanent hang, because the port prompt blocked on a terminal nobody had been
told to touch. The window is never closed now, the long steps stream as they run, the Docker check names
which of the three causes you have and quotes what the distro reported, and a failed setup tells you the
exit code, where to look, and the exact command that resumes it.

### New: choose your ports, and uninstall safely
Installing alongside something that already holds a default port no longer means a raw bind error: the
installer offers the next free port, and you can pin any of them. Uninstall is safe — it stops what it
started and never kills processes by name. A host-resident instance no longer reports unhealthy forever,
and a runtime daemon restart reaps only its own instance's containers.

### New: Iteration works in its own worktree
Iteration mode now does its work in its own git worktree, on one branch per conversation, rather than in
your checkout — so your working state, your uncommitted edits and your current branch are never where an
agent's half-finished change lands. It cleans up after itself once the work has landed, and never touches
another session's branch. Sharing your checkout stays possible; this is the default.

### New: work items you can skip on purpose
`skipped` was already a real status — the sequence engine consumed it, and it carried the meaning
"deliberately not doing this" — but nothing could ever set it: the update validator rejected it, and the
board and detail controls treated it as immovable. So the honest expression of that decision had to be a
*cancellation*, which reads like an accident and loses the distinction entirely. It is now settable by
hand, on both surfaces, through the same generic update path as every other status.

### Also in this release
- Paste into a permission card's free-text row works again.
- `ctrl+x` on a work item is the GUI's DELETE, not a soft cancel.
- A work-item search reaches matches inside collapsed nodes.
- A worker version's `concurrency_limit` is enforced. It was stored, plumbed and displayed everywhere and
  read on no dispatch path, so a limit of 1 still ran several at once; a dispatch that queues behind the
  limit now says why it is waiting instead of looking stuck.
- Backups work on a host-resident instance — the configuration a fresh install now produces. `pg_dump` was
  being looked up on the host rather than in the container that has it, so *every* backup on such an install
  failed with `executable file not found in $PATH`.
- The transcript's activity line stays in step with the turn: its "newest call Ns ago" age no longer counts
  up forever after a refresh, and the tool tally no longer freezes for a turn this client is not streaming.

## v0.5.10

### New: A permission card waits for you, however long you are away
The card that asks whether a call may proceed used to give up on you. Leave your desk with a question on
screen and the wait would expire: the call was refused, and the card you came back to could no longer be
clicked or selected — because every gesture needs the ask to still be **open**. Three separate quiet-time
deadlines did that, and all three are gone. The reply window now re-arms while a turn is parked on a card;
the registry sweep skips a conversation with an open ask; and the engine's own per-permission wait no
longer has a bound at all. A card is bounded by *you* now — you answer it, stop the turn, or send a new
message — so falling asleep at the keyboard costs you the card no longer. An operator who wants the old
leash back can set `ORCHICON_ASK_CONSENT_WAIT`, which restores the fail-closed expiry exactly as it was.

### New: Paste into a card's answer
`ctrl+v` into an ask card's **Other** row works, which it never did. The shell's paste path knew about
forms but not about a card's free-text row, so the key fell through to the composer and your text landed in
the message instead of the row you were looking at. Pasting an error trace or a code block straight into an
answer is the whole point, so **line breaks are preserved** — a card row holds your words, not a structured
field. The same works for an answered question's draft row.

### New: The installer catches up to host residency — and the ports are yours to pick
Host residency became the default shape in 0.4; the one-command installer now produces the *same* shape as
the launcher, so an instance no longer depends on which entry point created it. The installer also asks
about ports instead of assuming: it resolves every published port per instance, **prompts** on a conflict
(reading your terminal, so a piped install can still ask), and takes a variable for any port that should be
pinned and skipped. `ORCHICON_STRICT_PORTS=1` makes a conflict fatal instead of moving a port, for an
install that must not silently shift. A third **`test`** instance column lets you run a throwaway beside
`dev` and `prod` without colliding with either.

### New: A safe uninstall, and no more killing by process name
The uninstall path no longer stops processes by *name* — which could match something that had nothing to do
with Orchicon — and it will not repoint a launcher it did not create. Migrating an instance's data now
happens **before** its container exists, so a first switch-over cannot leave a plane booting against an
empty directory. A host-resident instance also stops reporting itself unhealthy once its services are up.

### Also in this release
- **Backups work on a host-resident install.** They did not: the dump shelled out to a `pg_dump` that only
  exists inside the Postgres container, so every backup on a host-resident instance failed with
  `executable file not found in $PATH`. The tool is now run where it actually lives, with the port
  translated for the container, and it prefers the container's own client so a version-skewed local one
  cannot fail the dump.
- **Work-item search reaches collapsed items.** The tree opens collapsed, and the filter was applied behind
  the collapse — so no query could reveal a match inside a folded parent, while the search box happily
  counted it. A query now suspends the collapse, keeps the ancestors of a match so the result holds its
  shape, and restores your collapse when cleared.
- **`ctrl+x` on a work item is the real delete.** It was the soft *cancel*, which left the cancelled row on
  screen — so deleting looked like it did nothing. It now performs the same permanent delete the GUI does,
  and the reversible status change is still one keystroke away in the status editor.
- **`skipped` is yours to set.** A terminal status the sequence engine consumes as *success* and passes
  over, so it is how you tell a chain "do not run this child" without cancelling it.
- **A runtime-daemon start reaps only its own instance's containers** — a dev start can no longer take
  prod's containers with it.
- **The per-worker concurrency limit is enforced**, and a dispatch that waits says why.
- **The website**: a contact section for every channel, a Contact link in the nav, and the install tabs
  fixed — clicking Windows or Docker used to change nothing, leaving the macOS/Linux command on screen.

## v0.5.0

### New: Claude Code is a first-class runtime — the platform runs on more than one engine now
Orchicon's second adapter lands at the same standard as the first, rather than beside it.
The Claude worker carries the same permission and sandbox posture — the never-allow class,
protected paths, the execution guard shim — the same stall, health, liveness and recovery
behaviour, the same todo-list contract, and the same telemetry and usage capture, so a run on
Claude is observable, recoverable and contained exactly like a run on the native engine. Its
stream is normalized onto the same execution callbacks every other runtime already used, which
is why the tool ledger, the diff surface and the recovery path needed no special case for it.
Claude's models are sourced from the catalog into the model picker in **both** clients, its
reasoning is carried through instead of dropped, and compaction runs on the shared budget
ladder. It is mounted from your own host install — never bundled — with an actionable preflight
that tells you what to run when the host is not signed in yet.

### New: MCP servers and skill files belong to a scope, not to the tenant
Every MCP server definition now belongs to exactly one scope.
A definition is **owner-scoped**: it belongs to one project, one Ask conversation, or one worker
version, and resolution is **one union** of the project's definitions and the scope's own. There
is no shared tenant list to inherit from, and no hidden precedence chain. The tenant-level MCP
surfaces are gone. What you get for that is legibility: what a session can reach is a property of *where
it runs*, visible on the same page that owns it. Attaching a server **is** the approval, so an
MCP tool no longer raises a consent card; a credential is **selected** from the store rather
than pasted into a form; and a run's MCP and skill demand is resolved once, when its container
is created, so what ran is what was configured. The honest consequence is stated rather than
hidden — a server can no longer be defined once and inherited by several projects, and the
catalog's one-click add per scope is the mitigation.

### New: A live activity line that says what a turn is actually doing
A long turn used to be indistinguishable from a stalled one. The thinking indicator now says
what is actually happening, and both clients render the same single row from the same rule: an unbroken answer states what the agent is doing with a
**rotating verb** and a **rolling tool counter** ("3 modifies · 1 read · last 4s"), a dropped
socket says it is reconnecting, and a turn that has genuinely gone quiet states the watchdog's
verdict instead of silently spinning. The verb list and the counter are pinned **cross-client**
against one fixture and indexed on the **same server clock**, so the browser and the terminal
report the same turn the same way. The line survives the arrival of the first token (it is
gated on the turn, not on "before any content") and yields to an escalation rather than glowing
through one — a turn that made five calls and then died escalates, as it must.

### New: The diff surface, finished in both clients
The diff panel now shows the whole change instead of hiding parts of it.
Long lines wrap rather than clip, the file lists are viewported rather than rendered whole, the
collapse state is measured rather than guessed, and the scrollbar is one you can actually grab.
Getting there meant fixing why it felt heavy: the terminal rendered the diff once per **mouse
event** rather than once per change, which is what made dragging it lag. Both clients were then
verified end to end against a live plane — in the GUI and the TUI, on both mounts.

### New: Quality of life across both clients
A run of smaller changes that alter how the clients feel to use, in both of them.
Panes resize. The browser's diff rail drags to any width and remembers it **per page**; in the
terminal every tree/detail split is adjustable, the diff rail resizes by drag **and** by
keyboard, and its width persists between sessions. The terminal is also **project-aware** now —
the conversations rail and the theme are scoped to the project workspace you are in, so
switching projects switches your context with you. Terminal list surfaces are real modals,
bordered and centred over a darkened backdrop, the schedules list shows both running and
finished times, and the composer advertises `ctrl+←/→` wherever there is a split to move.

### New: Ask turns stop losing work — and stop blaming the wrong thing
Most of this cycle is a set of defects that only real use surfaces, and they share one cause.
A turn holding a consent card is no longer a stalled turn, a shell command the
plane is *running* is no longer a wedged tool, and a refused clarifying question is no longer
read as your answer. A turn cancelled between tool rounds keeps the work it produced, an
aborted or superseded turn's reply is committed to the session so the model can see what it
just said, and the session history is **appended to** rather than replaced — a supersede used to
destroy the other turn's messages. A session can no longer lose a message in silence, a failed
turn says why, and the setting that governs all of it survives a partial save.

### Also in this release

- **Providers and local models:** transposed local-model URLs, discoverable Ask cards, a static Heads-Up grid, and no client-side auto-start gate.
- **Work items:** the Archive view is a real hierarchical tree, and a one-shot archive can be retried instead of failing silently.
- **Documentation:** a new `USERGUIDE.md` covers installing Orchicon and using **every screen in both clients**, and the in-app Settings → User Guide tab is gone with it — one guide, in one place, that cannot drift from a second copy of itself.
- **Fixes:** work-item schedule clearing and auto-start from any status, telemetry, credits and favicon corrections, and the TUI's spacebar behaving itself.


## v0.4.5

> **This release replaces v0.4.0.** 0.4.0 introduced the feature set further down and was
> withdrawn the same day: a single malformed tool call could take the whole control plane down,
> and that should not be the version anyone downloads. Nothing was dropped in the fix — v0.4.5
> is the first release of the 0.4 line to carry the features and the fixes in one download, and
> it is what `install` now gives you.

### New: The control plane cannot be taken down by one bad tool call
Ask Orchicon's `update_work_item` tool dereferenced a workflow id that a scheduled sequence parent holds as NULL **by construction** — a shape the API routes to the sequence chain, but which this path skipped. The panic then ran on a background goroutine that the HTTP server's per-connection recovery does not cover, so a single malformed tool call took down the **whole control plane** instead of failing that one call. Three of them are in this instance's own log. It is fixed — and so is the reason it was nearly impossible to find from the outside: the plane runs on the host, so the panic is written to the instance's own serve log (`$ORCHICON_SERVE_STATE_DIR/logs/orchicon.log`) and never appears in `docker logs`.

### New: A long conversation no longer makes the terminal unresponsive
Reading a 250-message conversation re-laid out **every** message on **every** frame of **every** pane — markdown, styling and box drawing for each — in order to draw the handful of rows that actually fit on screen. Measured, on the frame the client redraws while doing nothing else: 7.52 ms and 132,779 allocations, down to **266 µs and 652 allocations** (28× faster, 200× fewer allocations); 20× while streaming. Each message's rendering is now remembered until something it reads genuinely changes, and an accidental quadratic — counting an item's lines by scanning to the end of the accumulated transcript, once per item — went with it.

### New: A timeout measures silence, not age — and a question says what it is waiting for
Two separate limits measured how **long** a turn had been running, so the more honest work a turn was doing, the more likely it was to be killed at thirty minutes and the failure blamed on the model — naming a model that was perfectly fine. Both are inactivity bounds now: a turn that is still working is never killed for its age, and a turn that has genuinely gone quiet still ends, with the same honest message. A turn parked on a question of **yours** is now reported as exactly that, and the question is recorded as *unanswered* rather than as a refusal — so coming back and answering it resumes the conversation, instead of the question having to be asked again.

### New: A compacted conversation says so, and keeps what the work depends on
A long conversation is periodically summarized so it fits the model's window, and until now that happened **silently**: the only record was a line in the server log, where one conversation collapsed 2,343 messages into a single summary with nothing in the transcript to explain why the assistant no longer remembered what had been said. The collapse is now recorded in the conversation itself, in both clients. Two things that made it lossier than it looked are addressed too. The identifiers the work depends on — entity ids, file paths, commit names, the tools used — are extracted **before** the collapse and carried through verbatim, because the part of the transcript they lived in (tool arguments and tool output) is dropped before the summarizer ever sees it. And a history too large to save is compressed rather than silently not saved at all, which is what used to leave a long conversation reverting to a stale copy of itself after a restart.

### New in the 0.4 line

0.4.0 shipped these five features and was then withdrawn. They are unchanged here — listed under their own heading so it is clear which part of this release is the 0.4 feature set and which part is what followed it.

### New: Ask Orchicon can do the work — and asks before it does
Ask Orchicon was a conversation you could read your project *with*; it now runs the same file and shell suite the workers use, against your real filesystem, scoped to the conversation's project. Nothing that writes or executes happens without your say-so: each one opens a card in whichever client you are in, naming the tool and the exact target. A **session grant** covers a directory and everything beneath it for that conversation, and the row says so — naming the directory it would cover rather than leaving you to guess its reach. A persistent **deny list** (your SSH keys, cloud credentials, `gh` config, `.netrc`, Docker config) is absolute, and the destructive class — `sudo`, `dd`, `mkfs*`, partition and LVM tooling — can never be approved by anyone, including you. Reads never ask. The prompt describes that boundary as it really is, including what it does not cover.

### New: FULLSEND — stop the prompts deliberately, rather than by accident
A gate that cannot be opened on purpose gets bypassed by accident: mid-task, approving card after card, you stop reading them. FULLSEND is the honest version of that — one explicit, revocable mode per conversation, shown as a badge in the terminal composer and a dropdown in the browser. It waives the *prompt* and nothing else: an entry on your deny list still refuses, and the never-allow class is still unreachable. It lives in memory, so a fresh plane starts with it off and a bypass cannot outlive the session you enabled it in; it is recorded in the audit trail; it can be toggled mid-turn; and turning it on approves a permission card already on screen rather than leaving the turn waiting on it.

### New: A question pauses the turn instead of talking to itself
Asking a clarifying question used to be record-and-continue — the model wrote the question down, kept going, and your answer arrived as an unrelated message. The call **blocks** now: the question appears as a card, the turn waits exactly where it was, and what you answer becomes the tool's result, so the model resumes holding your words rather than guessing what they referred to.

### New: The plane runs on your host, with the services containerized
Host residency is the default shape: the control plane runs as a host process while Postgres, NATS and the Grafana telemetry stack stay in one container reached over loopback. It is the same install and the same binary — what changes is that the plane's runtime, file access and process tree are the host's rather than a container's. The rollback is one word, and each instance (`dev`, `prod`) chooses its shape independently, so one can migrate while the other does not.

### New: Orchicon will not destroy the directory it is working in

A command that would delete the project it is running in — or the directory holding it — is now **refused outright, and no approval can override it**. `rm -rf /home` from a project, `rm -rf ~`, `rm -rf /`, and a `rm -rf` of anything that *contains* the project (or a directory you granted) are all refused, on top of your deny list rather than instead of it.

THIS IS A DELIBERATE BEHAVIOUR CHANGE, and it is the one thing in this release that can refuse a
command you did not explicitly forbid. It exists because the alternative was worse: with FULLSEND on,
the sandbox's usual checks are waived *by design* — that is what the mode is for — and an ancestor of
the project was covered by none of them, so the gate that stops you approving card after card also
stood aside for the one command that takes everything with it.

Working *on* the project is untouched: deleting a build directory, clearing `dist`, a recursive
`chmod` on the project root all still work, because acting on the thing you opened is ordinary work.
What is refused is destroying the thing that *holds* it. Pressing FULLSEND does not lift this, and
neither does a session grant — see *Enforcement, not just prompting* in the documentation for the
rule and the two lists it uses.

### Also in this release

- **The terminal client's ask card offers "Other", the way the browser's does.** The recorded "Orchicon asks" card had no free-text row at all — only a footer pointing at the composer — and clicking the row sent the literal word `Other` as the answer. The row is on the card now, and what you type is sent as your next message.
- **A path that would destroy the scope can never be approved.** On top of the deny list and the never-allow class: a request whose target is a directory that contains the project you are working in is refused outright, by nobody's approval.
- **The Windows builds compile, so a release can actually be published.** The 0.4.0 cut could not produce its release assets; the build matrix is green across every platform it ships to.
- **The build no longer depends on your shell profile.** The makefile resolves its own copy of the schema tooling instead of trusting whatever `PATH` happens to contain at the moment you build.
- **Docs CI validates Mermaid diagrams**, so a diagram no other check can see cannot silently break in the published documentation.
- **The one-command installer no longer deletes YOUR directories.** `--force-clean` (and `--nuke`) removed `data`, `.dev` and `bin` as **relative names**, and the installer never changed directory — so they resolved against wherever you happened to be standing. Anyone who ran the documented command from inside a project lost *that project's* `bin/` and `data/`. It is anchored to Orchicon's own state directory now.
- **A run that executes in your working tree no longer discards your uncommitted work.** Tidying a shared checkout after a run ran `git reset --hard` and `git clean -fd`, and its only signal was "the checkout is dirty" — which is exactly what your own unsaved edits look like. Your work is stashed first and recoverable from `git stash list`, and if it cannot be stashed the tidying is skipped rather than the work being lost.
- **`make clean-docker` only touches Orchicon's containers.** It used to prune stopped containers and unused volumes across the whole Docker host, removing other projects' containers and data on any machine with more than Orchicon on it.
- **A card settles for every client, and survives a reload.** Answering in the terminal settles the same question in the browser, in a second tab, and after a page reload — the resolution is written into the turn's durable record rather than only broadcast to whoever happened to be watching at that moment.
- **Refusals say what actually happened.** A timeout is *expired* rather than an operator denial; an unreadable policy file is reported as a policy problem; a rule that refuses a call is attributed to the rule, not to an operator who was never asked. It matters because the model reads the reason and decides what to do next from it.
- **A turn that dies mid-work keeps its work.** Streaming reasoning was never finalized, so an interrupted turn lost the thinking entirely — and a completed answer discarded it even on a clean turn. Both now survive in the record.
- **Ephemeral runs recover.** A run whose git strategy is `none` creates no branch to resume onto, and was retried blindly; recovery now recognises that shape instead of failing it.
- **Settings: blank means the built-in default, and `0` means disabled.** They were the same value, so leaving a field blank could silently switch a control off.
- **Installer:** a WSL distro name containing a NUL byte no longer corrupts the generated config, and an empty variable expands safely.
- **Terminal client:** the Schedules lenses order history the way the browser does and derive queued sequence children; a send the server refuses because a turn is already running is delivered rather than bounced; the detail pane's paint and the terminal's colour profile are resolved rather than assumed.

## v0.4.0

### New: Ask Orchicon can do the work — and asks before it does
Ask Orchicon was a conversation you could read your project *with*; it now runs the same file and shell suite the workers use, against your real filesystem, scoped to the conversation's project. Nothing that writes or executes happens without your say-so: each one opens a card in whichever client you are in, naming the tool and the exact target. A **session grant** covers a directory and everything beneath it for that conversation, and the row says so — naming the directory it would cover rather than leaving you to guess its reach. A persistent **deny list** (your SSH keys, cloud credentials, `gh` config, `.netrc`, Docker config) is absolute, and the destructive class — `sudo`, `dd`, `mkfs*`, partition and LVM tooling — can never be approved by anyone, including you. Reads never ask. The prompt describes that boundary as it really is, including what it does not cover.

### New: FULLSEND — stop the prompts deliberately, rather than by accident
A gate that cannot be opened on purpose gets bypassed by accident: mid-task, approving card after card, you stop reading them. FULLSEND is the honest version of that — one explicit, revocable mode per conversation, shown as a badge in the terminal composer and a dropdown in the browser. It waives the *prompt* and nothing else: an entry on your deny list still refuses, and the never-allow class is still unreachable. It lives in memory, so a fresh plane starts with it off and a bypass cannot outlive the session you enabled it in; it is recorded in the audit trail; it can be toggled mid-turn; and turning it on approves a permission card already on screen rather than leaving the turn waiting on it.

### New: A question pauses the turn instead of talking to itself
Asking a clarifying question used to be record-and-continue — the model wrote the question down, kept going, and your answer arrived as an unrelated message. The call **blocks** now: the question appears as a card, the turn waits exactly where it was, and what you answer becomes the tool's result, so the model resumes holding your words rather than guessing what they referred to.

### New: The plane runs on your host, with the services containerized
Host residency is the default shape: the control plane runs as a host process while Postgres, NATS and the Grafana telemetry stack stay in one container reached over loopback. It is the same install and the same binary — what changes is that the plane's runtime, file access and process tree are the host's rather than a container's. The rollback is one word, and each instance (`dev`, `prod`) chooses its shape independently, so one can migrate while the other does not.

### New: Orchicon will not destroy the directory it is working in

A command that would delete the project it is running in — or the directory holding it — is now **refused outright, and no approval can override it**. `rm -rf /home` from a project, `rm -rf ~`, `rm -rf /`, and a `rm -rf` of anything that *contains* the project (or a directory you granted) are all refused, on top of your deny list rather than instead of it.

THIS IS A DELIBERATE BEHAVIOUR CHANGE, and it is the one thing in this release that can refuse a
command you did not explicitly forbid. It exists because the alternative was worse: with FULLSEND on,
the sandbox's usual checks are waived *by design* — that is what the mode is for — and an ancestor of
the project was covered by none of them, so the gate that stops you approving card after card also
stood aside for the one command that takes everything with it.

Working *on* the project is untouched: deleting a build directory, clearing `dist`, a recursive
`chmod` on the project root all still work, because acting on the thing you opened is ordinary work.
What is refused is destroying the thing that *holds* it. Pressing FULLSEND does not lift this, and
neither does a session grant — see *Enforcement, not just prompting* in the documentation for the
rule and the two lists it uses.

### Also in this release

- **The one-command installer no longer deletes YOUR directories.** `--force-clean` (and `--nuke`) removed `data`, `.dev` and `bin` as **relative names**, and the installer never changed directory — so they resolved against wherever you happened to be standing. Anyone who ran the documented command from inside a project lost *that project's* `bin/` and `data/`. It is anchored to Orchicon's own state directory now.
- **A run that executes in your working tree no longer discards your uncommitted work.** Tidying a shared checkout after a run ran `git reset --hard` and `git clean -fd`, and its only signal was "the checkout is dirty" — which is exactly what your own unsaved edits look like. Your work is stashed first and recoverable from `git stash list`, and if it cannot be stashed the tidying is skipped rather than the work being lost.
- **`make clean-docker` only touches Orchicon's containers.** It used to prune stopped containers and unused volumes across the whole Docker host, removing other projects' containers and data on any machine with more than Orchicon on it.

- **A card settles for every client, and survives a reload.** Answering in the terminal settles the same question in the browser, in a second tab, and after a page reload — the resolution is written into the turn's durable record rather than only broadcast to whoever happened to be watching at that moment.
- **Refusals say what actually happened.** A timeout is *expired* rather than an operator denial; an unreadable policy file is reported as a policy problem; a rule that refuses a call is attributed to the rule, not to an operator who was never asked. It matters because the model reads the reason and decides what to do next from it.
- **A turn that dies mid-work keeps its work.** Streaming reasoning was never finalized, so an interrupted turn lost the thinking entirely — and a completed answer discarded it even on a clean turn. Both now survive in the record.
- **Ephemeral runs recover.** A run whose git strategy is `none` creates no branch to resume onto, and was retried blindly; recovery now recognises that shape instead of failing it.
- **Settings: blank means the built-in default, and `0` means disabled.** They were the same value, so leaving a field blank could silently switch a control off.
- **Installer:** a WSL distro name containing a NUL byte no longer corrupts the generated config, and an empty variable expands safely.
- **Terminal client:** the Schedules lenses order history the way the browser does and derive queued sequence children; a send the server refuses because a turn is already running is delivered rather than bounced; the detail pane's paint and the terminal's colour profile are resolved rather than assumed.

## v0.3.1

### New: orchicon.dev, rebuilt around the product instead of around the pitch
The landing page no longer describes the platform in the abstract — it shows it. Side-by-side tours of the web app and the terminal client, each with real screenshots rather than mockups, the six steps that take an idea to a merged pull request, the three Ask modes the platform actually enforces, scheduled market research and the Idea Cloud, and budgets as a constraint rather than a report. It is also self-contained: one HTML file with inline styles and the marks inline as SVG, so it loads fast and has no build step beyond copying the installers in.

### New: A real brand in the app — the mark, and a favicon that actually exists
The app's logo was a placeholder: a rounded square with a gradient and a generic icon, which reads as unfinished rather than as Orchicon. Its favicon was worse — it pointed at a file that has never existed on disk, so every page load requested a 404 and no browser ever showed an icon in a tab, bookmark, or history entry. Both are now the Orchicon mark, drawn from the same source the website uses. It follows the theme rather than being a fixed colour: the five leading segments inherit the surrounding text, so the mark renders as ink on a light theme and as light on a dark one, with the closing segment keeping the brand signal green.

### Also in this release

- **The terminal client's panes stop showing coloured whitespace.** A detail pane pads short lines through a viewport, which writes one spelling of a "reset" escape, while the repair that re-asserts the pane's background only knew the other — so the padding was left unpainted and rendered in the *terminal's* colour instead of the pane's. Every markdown line shorter than the pane showed it: a heading, a bullet's last wrap, a paragraph's tail. It was never specific to one theme, which is why it looked wrong on all of them.

## v0.3.0

### New: OpenCode is optional — Orchicon runs on its own engine
Orchicon no longer requires an external runtime CLI: its own native engine runs sessions inside the control plane, and the OpenCode serve is started **only when something actually needs it**. Before this release, OpenCode was a hard prerequisite — install the CLI or nothing runs, and on a host without it the installer refused. Now the plane computes its own adapter demand set from the model refs you actually use, and a plane that needs no OpenCode never probes for the binary, never starts a serve, and never mounts it into a container. The model picker defaults to the native engine, container mounts are decided per run rather than per platform, and the installer no longer insists you install a runtime. **You can run Orchicon end to end today with no adapter CLI installed at all** — and if you already use OpenCode, nothing about your setup changes.

### New: A complete terminal client — the whole product in the TUI
`orch` is no longer a launcher alongside the GUI: it is a full client, with read *and* write parity across **all seven domains**. Ask Orchicon, Overview, Work, Execution, Automation, Enforcement and Control are all first-class from the terminal — creating, editing, publishing, approving, cancelling, retrying, bulk operations and live streams included. It has a real boxed multi-line composer with slash commands and a command palette, a slide-out diff sidebar, click-to-copy on your own messages, and **43 themes** across light and dark — including **true transparency**, where the terminal shows through the whole client and the text adapts to your terminal's own background so it stays readable.

### New: Work is workflow-first — every run is a workflow
Every run is now a workflow: standalone dispatch is retired, so a work item with no bound workflow cannot be scheduled, and the platform tells you when you create it rather than failing later at run time. A whole backlog bound to nothing used to be silently stranded. Binding a **workflow and a runtime image** is now a bulk operation across a selection, so unblocking a backlog is one gesture instead of a form per item, and the editor can no longer quietly clear a binding you did not touch.

### New: Ask Orchicon — enforced modes, real context management, and nothing lost
Its three modes are now enforced by the **platform**, not requested in prose: the tools a mode may not use are withheld from it and refused at the point of execution, so "Brainstorm will not write your files" is a refusal a model cannot talk its way past — and the boundary is adapter-agnostic, so a new runtime inherits it. Conversations manage their own context: compaction on demand, a proactive pressure gate, and recovery from overflow instead of a permanently wedged conversation. Every message and action is persisted **live**, so a timeout, abort or dropped socket no longer loses a turn, and Ask runs on **any** adapter rather than only OpenCode.

### New: Runs execute in containers, and the plane heals itself
Worker executions run in an isolated container per workflow run, drawn from a warm pool so dispatch never cold-starts, and reset between runs so no state crosses a boundary. A native fast path removed a three-minute dispatch stall. Stale runtime daemons and leaked containers are reaped, containers a run does not need are no longer mounted, and liveness probes no longer kill healthy workers. Runs survive backend failures instead of wedging.

### New: Quick Work hands off end to end
The dispatch mode whose whole purpose is to hand work over now actually does it: it asks the model question, confirms git, and publishes the workflow and work item so a run fires — with the DevOps step opening and merging the pull request.

### Also in this release

- OpenCode is now **optional everywhere**: the installer no longer warns that a runtime CLI is missing, and the docs describe adapters as pluggable rather than required.
- **Scheduler and dispatch**: cancel and abort genuinely stop the model session; recovery survives DAG pass limits; PR-merge loops and orphaned branch references fixed; tool-wedge recovery no longer kills a live turn; a tool-hang is redirected instead of orphaning the worker.
- **Ask Orchicon sessions**: follow-ups resolve against the *execution's* adapter rather than the host; tool-call replay no longer 400s after a model switch; attachments deliver; the phantom "budget" workers reported in Ask is gone.
- **Terminal client**: a key that produced a send could be silently swallowed; clicking now places the caret on wrapped and scrolled text; a stale shell reference stopped every notice (and half of every theme switch) from landing; lazy screen loads, scroll preservation and tab focus corrected.
- **Diff pipeline and telemetry**: the file-edit ledger no longer reports empty on live runs; diffs are server-computed; per-event invalidations are coalesced; the outbox is throttled with retention so a chatty run cannot flood the database.
- **Runtime images**: build from the terminal with live logs, and set them across a selection in bulk.

## v0.2.0

### New: Autonomous research & the Idea Cloud — Orchicon finds the work, you approve it

Orchicon can now run its own product research and propose what to build next. Put it on a schedule and a three-worker crew goes to work on its own: the **Planner** surveys the market live (agent platforms, harnesses, adjacent categories), the **Analyst** verifies each candidate against external evidence, and the **Synthesizer** distills it all into feature proposals. Proposals don't go straight into your backlog — they land in the **Idea Cloud**, a triage board separate from real work. Each idea carries its evidence and the run that produced it; promote it to turn it into real work items, or dismiss it and it's remembered as rejected history so it never comes back. Workers can even check the cloud themselves before spawning, so duplicates can't pile up. Instead of maintaining a backlog by hand, Orchicon surfaces genuinely new, feature-sized opportunities from the outside world — and you stay the decision-maker.

### New: Cost discipline, built in — budgets that actually enforce

v0.2.0 treats token spend as a first-class constraint. Every execution now runs under configurable budget ceilings — tokens, dollars, wall-clock, tool calls — with sensible built-in defaults calibrated from real usage telemetry. When a session gets expensive, Orchicon trims its context and keeps going instead of resending an ever-growing history; a turn-count limit caps how often that can happen; and truly hard limits (wall clock, tool calls) stop a runaway worker outright. Cache re-sends no longer count as work, so a long task can't trip its token budget just by re-reading what it already knows. New custom tools back this up end-to-end: workers read, write, and search files in batches and consume bounded, token-friendly lists everywhere — the same discipline visible to you on every execution page (peak working set, cumulative spend, cache hit rate). Net effect: materially lower cost per run, with the guarantee that a stuck or chatty worker can't silently burn money.

### New: A sleeker, faster interface — on desktop and phone

The control plane UI has been redesigned for clarity and speed: a cleaner shell and navigation, category folders for organizing workers, workflows, and conversations, drag-and-drop that just works, and an expanded theme system — 20 hand-tuned themes across light and dark. The mobile experience has been reworked end-to-end: layouts, touch targets, and navigation all behave properly on a phone, so you can check runs, approve work, and triage ideas from anywhere. Execution pages now show honest numbers — real model context windows, working set vs. cumulative totals, live token and cost usage.

### Also in this release

- **Runtime reliability**: self-healing container pools (stale daemons and leaked containers eliminated), stale-binary detection, and runs that self-heal across backend failures instead of wedging.
- **Scheduler resilience**: cancel/abort actually stops the model session; recovery survives DAG pass limits; PR-merge loops and orphaned branch references fixed.
- **Automation pipeline hardening**: role-scoped plane access with deny-by-default security, loud failures instead of silent no-ops, and a dedicated Rejected view for the Idea Cloud.
- **Developer experience**: one-command full rebuild (`make rebuild-dev` / `rebuild-prod`), automatic version tagging on develop, BuildKit-cached container builds.

Plus bug fixes across the runtime, scheduler, frontend, and settings — the full itemized list is on the [GitHub release page](https://github.com/beardedparrott/Orchicon/releases).
